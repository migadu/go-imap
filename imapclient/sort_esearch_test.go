package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// An extended SORT is answered with ESEARCH (RFC 5267 §3.1). Servers built from
// this fork used to send an ESORT response instead, which is still accepted.
func TestSortExtendedResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid      bool
		response string // %s is the command's tag
		wantSeq  []uint32
		wantUIDs []imap.UID
	}{
		{
			// RFC 7162 §3.1.10, Example 20.
			name:     "ESEARCH",
			response: `* ESEARCH (TAG "%s") ALL 5,3,2,1 MODSEQ 1236`,
			wantSeq:  []uint32{5, 3, 2, 1},
		},
		{
			name:     "ESEARCH UID",
			uid:      true,
			response: `* ESEARCH (TAG "%s") UID ALL 30,10:11,20`,
			wantUIDs: []imap.UID{30, 10, 11, 20},
		},
		{
			name:     "legacy ESORT",
			response: `* ESORT (TAG %s) ALL 3,1,2`,
			wantSeq:  []uint32{3, 1, 2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()

			go func() {
				fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 SORT ESORT] ready\r\n")
				line, err := bufio.NewReader(serverConn).ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				fmt.Fprintf(serverConn, tc.response+"\r\n", tag)
				fmt.Fprintf(serverConn, "%s OK SORT completed\r\n", tag)
			}()

			client := imapclient.New(clientConn, nil)
			defer client.Close()

			options := &imapclient.SortOptions{
				SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDate}},
				SearchCriteria: &imap.SearchCriteria{},
				Return:         imap.SortOptions{ReturnAll: true},
			}
			var cmd *imapclient.SortCommand
			if tc.uid {
				cmd = client.UIDSort(options)
			} else {
				cmd = client.Sort(options)
			}
			data, err := cmd.Wait()
			if err != nil {
				t.Fatalf("Sort() = %v", err)
			}
			if !reflect.DeepEqual(data.SeqNums, tc.wantSeq) {
				t.Errorf("SortData.SeqNums = %v, want %v", data.SeqNums, tc.wantSeq)
			}
			if !reflect.DeepEqual(data.UIDs, tc.wantUIDs) {
				t.Errorf("SortData.UIDs = %v, want %v", data.UIDs, tc.wantUIDs)
			}
		})
	}
}
