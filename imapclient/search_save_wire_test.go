package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestSearchReturnSaveWire(t *testing.T) {
	for _, mode := range []struct {
		name, prefix string
		uid, multi   bool
		source       *imap.SearchSource
	}{
		{name: "SEARCH", prefix: "SEARCH"},
		{name: "UID_SEARCH", prefix: "UID SEARCH", uid: true},
		{name: "ESEARCH_implicit_selected", prefix: "ESEARCH", uid: true, multi: true},
		{name: "ESEARCH_explicit_selected", prefix: "ESEARCH IN (selected)", uid: true, multi: true, source: &imap.SearchSource{Selected: true}},
	} {
		for _, tc := range []struct {
			name    string
			options *imap.SearchOptions
			want    []string
		}{
			{name: "save_only", options: &imap.SearchOptions{ReturnSave: true}, want: []string{"SAVE"}},
			{name: "save_and_all", options: &imap.SearchOptions{ReturnSave: true, ReturnAll: true}, want: []string{"ALL", "SAVE"}},
			{name: "save_and_other_options", options: &imap.SearchOptions{ReturnSave: true, ReturnAll: true, ReturnMin: true, ReturnMax: true, ReturnCount: true}, want: []string{"ALL", "COUNT", "MAX", "MIN", "SAVE"}},
			{name: "nil_options"},
			{name: "zero_options", options: &imap.SearchOptions{}},
			{name: "all_without_save", options: &imap.SearchOptions{ReturnAll: true}, want: []string{"ALL"}},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				clientConn, serverConn := net.Pipe()
				defer clientConn.Close()
				defer serverConn.Close()
				_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
				_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
				wire := make(chan string, 1)
				done := make(chan error, 1)
				set, numbers, min, max := "1:2", "1 2", uint32(1), uint32(2)
				if mode.uid {
					set, numbers, min, max = "11,13", "11 13", 11, 13
				}
				go func() {
					done <- func() error {
						br := bufio.NewReader(serverConn)
						if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 IMAP4rev2 ESEARCH SEARCHRES MULTISEARCH] ready\r\n"); err != nil {
							return err
						}
						line, err := br.ReadString('\n')
						if err != nil {
							return err
						}
						tag, _, _ := strings.Cut(line, " ")
						if _, err := fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", tag); err != nil {
							return err
						}
						line, err = br.ReadString('\n')
						if err != nil {
							return err
						}
						tag, command, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
						wire <- command
						if len(tc.want) == 0 && !mode.multi {
							if _, err := fmt.Fprintf(serverConn, "* SEARCH %s\r\n", numbers); err != nil {
								return err
							}
						} else {
							var fields []string
							if len(tc.want) == 0 || tc.options.ReturnAll {
								fields = append(fields, "ALL "+set)
							}
							if tc.options != nil && tc.options.ReturnMin {
								fields = append(fields, fmt.Sprintf("MIN %d", min))
							}
							if tc.options != nil && tc.options.ReturnMax {
								fields = append(fields, fmt.Sprintf("MAX %d", max))
							}
							if tc.options != nil && tc.options.ReturnCount {
								fields = append(fields, "COUNT 2")
							}
							// SAVE by itself suppresses the search response.
							if len(fields) > 0 {
								correlator, uid := fmt.Sprintf("TAG %q", tag), ""
								if mode.multi {
									correlator += ` MAILBOX "INBOX" UIDVALIDITY 1`
								}
								if mode.uid {
									uid = " UID"
								}
								if _, err := fmt.Fprintf(serverConn, "* ESEARCH (%s)%s %s\r\n", correlator, uid, strings.Join(fields, " ")); err != nil {
									return err
								}
							}
						}
						if _, err := fmt.Fprintf(serverConn, "%s OK search completed\r\n", tag); err != nil {
							return err
						}
						line, err = br.ReadString('\n')
						if err != nil {
							return err
						}
						tag, _, _ = strings.Cut(line, " ")
						_, err = fmt.Fprintf(serverConn, "%s OK still framed\r\n", tag)
						return err
					}()
				}()
				c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
				defer c.Close()
				if err := c.WaitGreeting(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					t.Fatal(err)
				}
				var data *imap.SearchData
				var err error
				if mode.multi {
					var results []*imap.SearchData
					results, err = c.MultiSearch(mode.source, &imap.SearchCriteria{}, tc.options).Wait()
					if len(results) > 0 {
						if len(results) != 1 || results[0].Mailbox != "INBOX" || results[0].UIDValidity != 1 {
							t.Fatalf("MultiSearch results = %+v", results)
						}
						data = results[0]
					}
				} else if mode.uid {
					data, err = c.UIDSearch(&imap.SearchCriteria{}, tc.options).Wait()
				} else {
					data, err = c.Search(&imap.SearchCriteria{}, tc.options).Wait()
				}
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				assertSearchReturnOptionsWire(t, <-wire, mode.prefix, tc.want)
				wantAll := len(tc.want) == 0 || tc.options.ReturnAll
				if wantAll && (data == nil || data.All == nil || data.All.String() != set) {
					t.Errorf("search result = %+v, want ALL %s", data, set)
				}
				if tc.options != nil && tc.options.ReturnCount && (data == nil || data.Count != 2 || data.Min != min || data.Max != max) {
					t.Errorf("search result = %+v, want COUNT 2 MIN %d MAX %d", data, min, max)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatalf("NOOP: %v", err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func assertSearchReturnOptionsWire(t *testing.T, command, prefix string, want []string) {
	t.Helper()
	rest, ok := strings.CutPrefix(command, prefix+" ")
	if !ok {
		t.Errorf("command = %q, want prefix %q", command, prefix)
		return
	}
	var got []string
	if options, hasReturn := strings.CutPrefix(rest, "RETURN ("); hasReturn {
		list, tail, closed := strings.Cut(options, ") ")
		if !closed {
			t.Errorf("command has an unclosed RETURN list: %q", command)
			return
		}
		got, rest = strings.Fields(list), tail
	}
	slices.Sort(got)
	if !slices.Equal(got, want) || rest != "ALL" {
		t.Errorf("command = %q: RETURN options %v, want %v; criteria %q, want ALL", command, got, want, rest)
	}
}
