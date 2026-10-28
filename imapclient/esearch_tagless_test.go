package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 4466 section 2.6.2 and RFC 9051 section 7.3.4: a missing correlator
// identifies an unsolicited response. It must be consumed without assigning
// its data to a command, even when exactly one command is pending.
func TestESearchWithoutCorrelator(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid      bool
		response string
	}{
		{name: "ALL", response: "* ESEARCH ALL 1:3"},
		{name: "UID ALL", uid: true, response: "* ESEARCH UID ALL 4,7"},
		{name: "COUNT", response: "* ESEARCH COUNT 3"},
		{name: "bare", response: "* ESEARCH"},
		{name: "bare UID", uid: true, response: "* ESEARCH UID"},
		{name: "MAILBOX without TAG", uid: true, response: `* ESEARCH (MAILBOX "INBOX" UIDVALIDITY 1) UID COUNT 3`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newESearchServer(t, tc.response)

			criteria := &imap.SearchCriteria{}
			options := &imap.SearchOptions{ReturnAll: true, ReturnCount: true}
			var cmd *imapclient.SearchCommand
			if tc.uid {
				cmd = client.UIDSearch(criteria, options)
			} else {
				cmd = client.Search(criteria, options)
			}
			data, err := cmd.Wait()
			if err != nil {
				t.Fatalf("Search() = %v", err)
			}
			var gotAll string
			if data.All != nil {
				gotAll = data.All.String()
			}
			if gotAll != "" || data.Count != 0 || data.Mailbox != "" || data.UIDValidity != 0 {
				t.Errorf("unsolicited ESEARCH populated SearchData: %+v", data)
			}
		})
	}

	t.Run("SORT", func(t *testing.T) {
		client := newESearchServer(t, "* ESEARCH ALL 3,1,2 COUNT 3")
		data, err := client.Sort(&imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
			SearchCriteria: &imap.SearchCriteria{},
			Return:         imap.SortOptions{ReturnAll: true, ReturnCount: true},
		}).Wait()
		if err != nil {
			t.Fatalf("Sort() = %v", err)
		}
		if len(data.SeqNums) != 0 || len(data.UIDs) != 0 || data.Count != 0 {
			t.Errorf("unsolicited ESEARCH populated SortData: %+v", data)
		}
	})
}

func TestESearchRequiresMatchingTag(t *testing.T) {
	for _, kinds := range [][]string{
		{"SEARCH"},
		{"SORT"},
		{"MULTISEARCH"},
		{"SEARCH", "SORT", "MULTISEARCH"},
	} {
		for _, response := range []struct{ name, wire string }{
			{"no correlator", "* ESEARCH COUNT 99"},
			{"MAILBOX without TAG", `* ESEARCH (MAILBOX "other" UIDVALIDITY 99) UID COUNT 99`},
			{"empty TAG", `* ESEARCH (TAG "") COUNT 99`},
			{"unknown TAG", `* ESEARCH (TAG "unknown") COUNT 99`},
		} {
			t.Run(strings.Join(kinds, "+")+"/"+response.name, func(t *testing.T) {
				client := newScriptedESearchServer(t, func(conn net.Conn, reader *bufio.Reader) {
					tags := make([]string, len(kinds))
					for i := range tags {
						line, err := reader.ReadString('\n')
						if err != nil {
							return
						}
						tags[i], _, _ = strings.Cut(line, " ")
					}
					// Replies arrive out of command order. Each correlated result
					// must survive a later unsolicited COUNT while all commands
					// remain pending.
					for i := len(tags) - 1; i >= 0; i-- {
						if kinds[i] == "MULTISEARCH" {
							fmt.Fprintf(conn, "* ESEARCH (MAILBOX \"INBOX\" UIDVALIDITY 1 TAG %q) UID COUNT %d\r\n", tags[i], 3+i)
						} else {
							fmt.Fprintf(conn, "* ESEARCH (TAG %q) COUNT %d\r\n", tags[i], 3+i)
						}
					}
					fmt.Fprint(conn, response.wire+"\r\n")
					for _, tag := range tags {
						fmt.Fprintf(conn, "%s OK done\r\n", tag)
					}
				})

				var waits []func() (uint32, error)
				for _, kind := range kinds {
					switch kind {
					case "SEARCH":
						cmd := client.Search(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnCount: true})
						waits = append(waits, func() (uint32, error) {
							data, err := cmd.Wait()
							return data.Count, err
						})
					case "SORT":
						cmd := client.Sort(&imapclient.SortOptions{
							SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
							SearchCriteria: &imap.SearchCriteria{},
							Return:         imap.SortOptions{ReturnCount: true},
						})
						waits = append(waits, func() (uint32, error) {
							data, err := cmd.Wait()
							return data.Count, err
						})
					case "MULTISEARCH":
						cmd := client.MultiSearch(nil, &imap.SearchCriteria{}, &imap.SearchOptions{ReturnCount: true})
						waits = append(waits, func() (uint32, error) {
							data, err := cmd.Wait()
							if err != nil {
								return 0, err
							}
							if len(data) != 1 || data[0].Mailbox != "INBOX" || data[0].UIDValidity != 1 {
								return 0, fmt.Errorf("unexpected MULTISEARCH results: %+v", data)
							}
							return data[0].Count, nil
						})
					}
				}
				for i, wait := range waits {
					count, err := wait()
					if err != nil {
						t.Fatalf("%s: %v", kinds[i], err)
					}
					if want := uint32(3 + i); count != want {
						t.Errorf("%s COUNT = %d, want %d", kinds[i], count, want)
					}
				}
			})
		}
	}
}

func TestESearchWithoutPendingCommand(t *testing.T) {
	sent := make(chan struct{})
	client := newScriptedESearchServer(t, func(conn net.Conn, reader *bufio.Reader) {
		fmt.Fprint(conn, "* ESEARCH\r\n* ESEARCH UID\r\n* ESEARCH COUNT 99\r\n"+
			"* ESEARCH (MAILBOX \"INBOX\" UIDVALIDITY 1) UID ALL 4,7 COUNT 2\r\n"+
			"* ESEARCH (TAG \"unknown\") COUNT 99\r\n")
		close(sent)
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		fmt.Fprintf(conn, "%s OK still connected\r\n", tag)
	})
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("server could not deliver unsolicited ESEARCH responses")
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("NOOP after unsolicited ESEARCH: %v", err)
	}
}

// newESearchServer answers one command with response and a tagged OK.
func newESearchServer(t *testing.T, response string) *imapclient.Client {
	t.Helper()
	return newScriptedESearchServer(t, func(conn net.Conn, reader *bufio.Reader) {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		fmt.Fprint(conn, response+"\r\n"+tag+" OK done\r\n")
	})
}

func newScriptedESearchServer(t *testing.T, serve func(net.Conn, *bufio.Reader)) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { serverConn.Close() })
	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 ESEARCH SORT ESORT MULTISEARCH] ready\r\n")
		serve(serverConn, bufio.NewReader(serverConn))
	}()
	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 5 * time.Second})
	t.Cleanup(func() { client.Close() })
	return client
}
