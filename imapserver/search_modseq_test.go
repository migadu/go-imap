package imapserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// RFC 7162 §3.1.5 and §3.1.6 add the highest mod-sequence to a SEARCH answer
// "if the client specifies a MODSEQ criterion". A client that never used
// CONDSTORE reads `"SEARCH" *(SP nz-number)` and cannot parse a suffix it did
// not ask for.
func TestSearchModSeqOnlyWithModSeqCriterion(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}, imap.CapESearch: {}, imap.CapMultiSearch: {}})
	rc.appendOne("one")
	rc.do("SELECT INBOX")

	for _, tc := range []struct {
		cmd        string
		wantModSeq bool
	}{
		{"SEARCH ALL", false},
		{"SEARCH RETURN (COUNT) ALL", false},
		{"SEARCH MODSEQ 1", true},
		{"SEARCH RETURN (COUNT) MODSEQ 1", true},
		{"SEARCH NOT MODSEQ 999", true},
		{"SEARCH OR SEEN MODSEQ 1", true},
		// RFC 7377 MULTISEARCH takes the same path to ESEARCH.
		{"ESEARCH IN (MAILBOXES INBOX) ALL", false},
		{"ESEARCH IN (MAILBOXES INBOX) MODSEQ 1", true},
	} {
		resp := rc.do(tc.cmd)
		if got := strings.Contains(resp, "MODSEQ"); got != tc.wantModSeq {
			t.Errorf("%s: MODSEQ in response = %v, want %v:\n%s", tc.cmd, got, tc.wantModSeq, resp)
		}
	}
}

// emptySearchWithModSeq is a backend that sets ModSeq even for an empty result.
type emptySearchWithModSeq struct{ imapserver.Session }

func (s *emptySearchWithModSeq) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	return &imap.SearchData{All: imap.SeqSet{}, ModSeq: 7}, nil
}

// RFC 7162 §7: `"SEARCH" [1*(SP nz-number) SP search-sort-mod-seq]`. The
// mod-sequence may only follow at least one number, whatever the backend sets.
func TestSearchModSeqNotSentForEmptyResult(t *testing.T) {
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}}, func(s imapserver.Session) imapserver.Session {
		return &emptySearchWithModSeq{s}
	})
	rc.appendOne("one")
	rc.do("SELECT INBOX")

	resp := rc.do("SEARCH MODSEQ 1")
	if strings.Contains(resp, "MODSEQ") || !strings.Contains(resp, "* SEARCH\r\n") {
		t.Errorf("empty SEARCH result answered:\n%s\nwant a bare * SEARCH", resp)
	}
}
