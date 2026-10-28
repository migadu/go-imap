package imapclient_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 5267 §3.1: MIN and MAX are the first and last messages in sort order.
// RFC 4731 §3.2, which RFC 7162 §3.1.10 applies to SORT: with MIN or MAX alone
// MODSEQ is that message's, with both and no ALL or COUNT the higher of the
// two, and otherwise the highest of all the messages returned.
func TestSort_ModSeqReturnOptions(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	const mboxName = "SORT-MODSEQ"
	if err := client.Create(mboxName, nil).Wait(); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if _, err := client.Select(mboxName, nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}
	for i := 0; i < 3; i++ {
		msg := []byte("Subject: m\r\n\r\nbody")
		cmd := client.Append(mboxName, int64(len(msg)), nil)
		if _, err := cmd.Write(msg); err != nil {
			t.Fatalf("Append.Write() = %v", err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("Append.Close() = %v", err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("Append.Wait() = %v", err)
		}
		time.Sleep(10 * time.Millisecond) // distinct arrival times
	}
	// Give the middle message (in ARRIVAL order) the highest mod-sequence.
	store := client.Store(imap.SeqSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil)
	if err := store.Close(); err != nil {
		t.Fatalf("Store() = %v", err)
	}
	msgs, err := client.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{ModSeq: true}).Collect()
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	modSeq := map[uint32]uint64{}
	for _, msg := range msgs {
		modSeq[msg.SeqNum] = msg.ModSeq
	}
	if !(modSeq[2] > modSeq[1] && modSeq[2] > modSeq[3]) {
		t.Fatalf("mod-sequences %v: want message 2 highest", modSeq)
	}

	for _, tc := range []struct {
		name       string
		reverse    bool
		ret        imap.SortOptions
		wantMin    uint32
		wantMax    uint32
		wantModSeq uint64
	}{
		{name: "MIN", ret: imap.SortOptions{ReturnMin: true}, wantMin: 1, wantModSeq: modSeq[1]},
		{name: "MAX", ret: imap.SortOptions{ReturnMax: true}, wantMax: 3, wantModSeq: modSeq[3]},
		{name: "MIN MAX", ret: imap.SortOptions{ReturnMin: true, ReturnMax: true}, wantMin: 1, wantMax: 3, wantModSeq: max(modSeq[1], modSeq[3])},
		{name: "ALL", ret: imap.SortOptions{ReturnAll: true}, wantModSeq: modSeq[2]},
		{name: "COUNT", ret: imap.SortOptions{ReturnCount: true}, wantModSeq: modSeq[2]},
		{name: "MIN COUNT", ret: imap.SortOptions{ReturnMin: true, ReturnCount: true}, wantMin: 1, wantModSeq: modSeq[2]},
		// REVERSE ARRIVAL sorts 3, 2, 1: the lowest sorted message is 3.
		{name: "REVERSE MIN", reverse: true, ret: imap.SortOptions{ReturnMin: true}, wantMin: 3, wantModSeq: modSeq[3]},
		{name: "REVERSE MAX", reverse: true, ret: imap.SortOptions{ReturnMax: true}, wantMax: 1, wantModSeq: modSeq[1]},
		{name: "REVERSE MIN MAX", reverse: true, ret: imap.SortOptions{ReturnMin: true, ReturnMax: true}, wantMin: 3, wantMax: 1, wantModSeq: max(modSeq[1], modSeq[3])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := client.Sort(&imapclient.SortOptions{
				SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: tc.reverse}},
				SearchCriteria: &imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 1}},
				Return:         tc.ret,
			}).Wait()
			if err != nil {
				t.Fatalf("Sort() = %v", err)
			}
			if data.Min != tc.wantMin || data.Max != tc.wantMax {
				t.Errorf("MIN %v MAX %v, want MIN %v MAX %v", data.Min, data.Max, tc.wantMin, tc.wantMax)
			}
			if data.ModSeq != tc.wantModSeq {
				t.Errorf("MODSEQ %v, want %v (message mod-sequences %v)", data.ModSeq, tc.wantModSeq, modSeq)
			}
		})
	}
}

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
