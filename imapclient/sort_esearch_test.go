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
// Either form may end with the highest mod-sequence (RFC 7162 §3.1.9, §3.1.10).
func TestSortResponse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		plain      bool // no RETURN options
		uid        bool
		response   string // %s is the command's tag
		wantSeq    []uint32
		wantUIDs   []imap.UID
		wantModSeq uint64
	}{
		{
			// RFC 7162 §3.1.10, Example 20.
			name:       "ESEARCH",
			response:   `* ESEARCH (TAG "%s") ALL 5,3,2,1 MODSEQ 1236`,
			wantSeq:    []uint32{5, 3, 2, 1},
			wantModSeq: 1236,
		},
		{
			// RFC 7162 §3.1.9, in the form of §3.1.5's Example 15.
			name:       "plain SORT with MODSEQ",
			plain:      true,
			response:   `* SORT 2 5 6 (MODSEQ 917162500)`,
			wantSeq:    []uint32{2, 5, 6},
			wantModSeq: 917162500,
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
				fmt.Fprint(serverConn, strings.ReplaceAll(tc.response, "%s", tag)+"\r\n")
				fmt.Fprintf(serverConn, "%s OK SORT completed\r\n", tag)
			}()

			client := imapclient.New(clientConn, nil)
			defer client.Close()

			options := &imapclient.SortOptions{
				SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDate}},
				SearchCriteria: &imap.SearchCriteria{},
			}
			if !tc.plain {
				options.Return = imap.SortOptions{ReturnAll: true}
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
			if data.ModSeq != tc.wantModSeq {
				t.Errorf("SortData.ModSeq = %v, want %v", data.ModSeq, tc.wantModSeq)
			}
		})
	}
}
