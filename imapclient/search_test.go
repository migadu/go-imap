package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestSearch(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	criteria := imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   "Message-Id",
			Value: "<191101702316132@example.com>",
		}},
	}
	data, err := client.Search(&criteria, nil).Wait()
	if err != nil {
		t.Fatalf("Search().Wait() = %v", err)
	}
	seqSet, ok := data.All.(imap.SeqSet)
	if !ok {
		t.Fatalf("SearchData.All = %T, want SeqSet", data.All)
	}
	nums, _ := seqSet.Nums()
	want := []uint32{1}
	if !reflect.DeepEqual(nums, want) {
		t.Errorf("SearchData.All.Nums() = %v, want %v", nums, want)
	}
}

func TestESearch(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapESearch) {
		t.Skip("server doesn't support ESEARCH")
	}

	criteria := imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   "Message-Id",
			Value: "<191101702316132@example.com>",
		}},
	}
	options := imap.SearchOptions{
		ReturnCount: true,
	}
	data, err := client.Search(&criteria, &options).Wait()
	if err != nil {
		t.Fatalf("Search().Wait() = %v", err)
	}
	if want := uint32(1); data.Count != want {
		t.Errorf("Count = %v, want %v", data.Count, want)
	}
}

func TestMultiSearch(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.Cap("MULTISEARCH")) {
		t.Skip("server doesn't support MULTISEARCH")
	}

	// Create archive mailbox and add the same message
	if err := client.Create("archive", nil).Wait(); err != nil {
		t.Fatalf("Create().Wait() = %v", err)
	}
	appendCmd := client.Append("archive", int64(len(simpleRawMessage)), nil)
	appendCmd.Write([]byte(simpleRawMessage))
	appendCmd.Close()
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("Append().Wait() = %v", err)
	}

	criteria := imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   "Message-Id",
			Value: "<191101702316132@example.com>",
		}},
	}
	options := imap.SearchOptions{
		ReturnCount: true,
	}
	source := &imap.SearchSource{Mailboxes: []string{"INBOX", "archive"}}
	dataList, err := client.MultiSearch(source, &criteria, &options).Wait()
	if err != nil {
		t.Fatalf("MultiSearch().Wait() = %v", err)
	}
	if len(dataList) != 2 {
		t.Fatalf("len(dataList) = %v, want 2", len(dataList))
	}
	if want := uint32(1); dataList[0].Count != want {
		t.Errorf("dataList[0].Count = %v, want %v", dataList[0].Count, want)
	}
	if want := "INBOX"; dataList[0].Mailbox != want {
		t.Errorf("dataList[0].Mailbox = %v, want %v", dataList[0].Mailbox, want)
	}
	if want := uint32(1); dataList[1].Count != want {
		t.Errorf("dataList[1].Count = %v, want %v", dataList[1].Count, want)
	}
	if want := "archive"; dataList[1].Mailbox != want {
		t.Errorf("dataList[1].Mailbox = %v, want %v", dataList[1].Mailbox, want)
	}
	// RFC 7377 §2.1: every multi-mailbox ESEARCH result must carry UIDVALIDITY.
	for i, data := range dataList {
		if data.UIDValidity == 0 {
			t.Errorf("dataList[%d].UIDValidity = 0, want non-zero (%q)", i, data.Mailbox)
		}
	}
}

// RFC 4731 §3.1:
// esearch-response = "ESEARCH" [search-correlator] [SP "UID"] *(SP search-return-data)
// Both correlated and uncorrelated ESEARCH responses (with or without UID and
// with or without return data items) are valid on the wire. An uncorrelated
// one was not caused by the pending command (RFC 4466 §2.6.2), so it must be
// read without desynchronising the stream and without landing in its result.
func TestESearchWire(t *testing.T) {
	tests := []struct {
		name     string
		uid      bool
		wireLine func(tag string) string
		wantUIDs []imap.UID
		wantSeqs []uint32
		wantMin  uint32
		wantMax  uint32
		wantCnt  uint32
	}{
		{
			name: "correlated UID with ALL",
			uid:  true,
			wireLine: func(tag string) string {
				return fmt.Sprintf("* ESEARCH (TAG %q) UID ALL 1:2\r\n", tag)
			},
			wantUIDs: []imap.UID{1, 2},
		},
		{
			name: "correlated UID empty",
			uid:  true,
			wireLine: func(tag string) string {
				return fmt.Sprintf("* ESEARCH (TAG %q) UID\r\n", tag)
			},
		},
		{
			name: "correlated seqnum empty",
			uid:  false,
			wireLine: func(tag string) string {
				return fmt.Sprintf("* ESEARCH (TAG %q)\r\n", tag)
			},
		},
		{
			name: "uncorrelated UID with ALL",
			uid:  true,
			wireLine: func(tag string) string {
				return "* ESEARCH UID ALL 1:2\r\n"
			},
		},
		{
			name: "uncorrelated UID with MIN MAX COUNT",
			uid:  true,
			wireLine: func(tag string) string {
				return "* ESEARCH UID MIN 1 MAX 2 COUNT 2\r\n"
			},
		},
		{
			name: "uncorrelated UID empty",
			uid:  true,
			wireLine: func(tag string) string {
				return "* ESEARCH UID\r\n"
			},
		},
		{
			name: "uncorrelated seqnum with ALL",
			uid:  false,
			wireLine: func(tag string) string {
				return "* ESEARCH ALL 1:2\r\n"
			},
		},
		{
			name: "uncorrelated seqnum empty",
			uid:  false,
			wireLine: func(tag string) string {
				return "* ESEARCH\r\n"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

			go func() {
				defer serverConn.Close()
				io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ESEARCH] ready\r\n")
				reader := bufio.NewReader(serverConn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					tag := fields[0]
					if strings.Contains(line, " SEARCH ") {
						io.WriteString(serverConn, tc.wireLine(tag))
					}
					fmt.Fprintf(serverConn, "%s OK completed\r\n", tag)
				}
			}()

			client := imapclient.New(clientConn, nil)
			t.Cleanup(func() { client.Close() })
			if err := client.WaitGreeting(); err != nil {
				t.Fatalf("WaitGreeting() = %v", err)
			}

			opts := &imap.SearchOptions{ReturnAll: true}
			var (
				data *imap.SearchData
				err  error
			)
			if tc.uid {
				data, err = client.UIDSearch(&imap.SearchCriteria{}, opts).Wait()
			} else {
				data, err = client.Search(&imap.SearchCriteria{}, opts).Wait()
			}
			if err != nil {
				t.Fatalf("Search().Wait() = %v", err)
			}

			if data.Min != tc.wantMin {
				t.Errorf("Min = %d, want %d", data.Min, tc.wantMin)
			}
			if data.Max != tc.wantMax {
				t.Errorf("Max = %d, want %d", data.Max, tc.wantMax)
			}
			if data.Count != tc.wantCnt {
				t.Errorf("Count = %d, want %d", data.Count, tc.wantCnt)
			}
			if tc.wantUIDs != nil {
				uidSet, ok := data.All.(imap.UIDSet)
				if !ok {
					t.Fatalf("SearchData.All = %T, want UIDSet", data.All)
				}
				got, _ := uidSet.Nums()
				if !reflect.DeepEqual(got, tc.wantUIDs) {
					t.Errorf("UIDs = %v, want %v", got, tc.wantUIDs)
				}
			}
			if tc.wantUIDs == nil && tc.wantSeqs == nil && data.All != nil && data.All.String() != "" {
				t.Errorf("SearchData.All = %v, want empty", data.All)
			}
			if tc.wantSeqs != nil {
				seqSet, ok := data.All.(imap.SeqSet)
				if !ok {
					t.Fatalf("SearchData.All = %T, want SeqSet", data.All)
				}
				got, _ := seqSet.Nums()
				if !reflect.DeepEqual(got, tc.wantSeqs) {
					t.Errorf("Seqs = %v, want %v", got, tc.wantSeqs)
				}
			}

			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop().Wait() after ESEARCH failed (stream desync): %v", err)
			}
		})
	}
}
