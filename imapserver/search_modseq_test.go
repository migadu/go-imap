package imapserver_test

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
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
