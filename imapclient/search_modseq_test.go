package imapclient_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// RFC 7162 §3.1.5: the mod-sequence is the highest of "all messages being
// returned", not of the mailbox. RFC 4731 §3.2 narrows it for MIN and MAX: with
// one of them alone it is that message's, with both and no ALL or COUNT the
// higher of the two.
func TestSearch_ModSeqOfReturnedMessages(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	modSeq := selectModSeqMailbox(t, client, "SEARCH-MODSEQ")
	all := imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 1}}
	// Messages 1 and 3: leaves out message 2, the mailbox's highest.
	unflagged := imap.SearchCriteria{
		ModSeq:  &imap.SearchCriteriaModSeq{ModSeq: 1},
		NotFlag: []imap.Flag{imap.FlagFlagged},
	}

	for _, tc := range []struct {
		name       string
		criteria   imap.SearchCriteria
		options    *imap.SearchOptions
		wantModSeq uint64
	}{
		{"MIN", all, &imap.SearchOptions{ReturnMin: true}, modSeq[1]},
		{"MAX", all, &imap.SearchOptions{ReturnMax: true}, modSeq[3]},
		{"MIN MAX", all, &imap.SearchOptions{ReturnMin: true, ReturnMax: true}, max(modSeq[1], modSeq[3])},
		{"MIN COUNT", all, &imap.SearchOptions{ReturnMin: true, ReturnCount: true}, modSeq[2]},
		{"ALL", all, &imap.SearchOptions{ReturnAll: true}, modSeq[2]},
		{"ALL of a subset", unflagged, &imap.SearchOptions{ReturnAll: true}, max(modSeq[1], modSeq[3])},
		{"plain SEARCH of a subset", unflagged, nil, max(modSeq[1], modSeq[3])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := client.Search(&tc.criteria, tc.options).Wait()
			if err != nil {
				t.Fatalf("Search() = %v", err)
			}
			if data.ModSeq != tc.wantModSeq {
				t.Errorf("MODSEQ %v, want %v (message mod-sequences %v)", data.ModSeq, tc.wantModSeq, modSeq)
			}
		})
	}
}
