package imapclient_test

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 7162 §3.1.9 and §3.1.10: a SORT whose criteria use MODSEQ is answered
// with the highest mod-sequence of the result, and one that does not use MODSEQ
// is answered without it.
func TestSort_ModSeq(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{ModSeq: true}).Collect()
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	modSeq := msgs[0].ModSeq

	for _, tc := range []struct {
		name       string
		criteria   imap.SearchCriteria
		wantSeq    []uint32
		wantModSeq uint64
	}{
		{
			name:       "MODSEQ",
			criteria:   imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: modSeq - 1}},
			wantSeq:    []uint32{1},
			wantModSeq: modSeq,
		},
		{
			name:       "MODSEQ under NOT",
			criteria:   imap.SearchCriteria{Not: []imap.SearchCriteria{{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: modSeq + 1}}}},
			wantSeq:    []uint32{1},
			wantModSeq: modSeq,
		},
		{
			name:    "no MODSEQ",
			wantSeq: []uint32{1},
		},
		{
			name:     "MODSEQ, no match",
			criteria: imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: modSeq + 1}},
		},
	} {
		for _, extended := range []bool{false, true} {
			name := tc.name
			if extended {
				name += " extended"
			}
			t.Run(name, func(t *testing.T) {
				options := &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
					SearchCriteria: &tc.criteria,
				}
				if extended {
					options.Return = imap.SortOptions{ReturnAll: true}
				}
				data, err := client.Sort(options).Wait()
				if err != nil {
					t.Fatalf("Sort() = %v", err)
				}
				if len(data.SeqNums) != 0 || len(tc.wantSeq) != 0 {
					if !reflect.DeepEqual(data.SeqNums, tc.wantSeq) {
						t.Errorf("SortData.SeqNums = %v, want %v", data.SeqNums, tc.wantSeq)
					}
				}
				if data.ModSeq != tc.wantModSeq {
					t.Errorf("SortData.ModSeq = %v, want %v", data.ModSeq, tc.wantModSeq)
				}
			})
		}
	}
}
