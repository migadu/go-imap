package imapserver_test

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// IMAP4rev2 changes SEARCH's answer to ESEARCH (RFC 9051 §6.4.4, Appendix E
// item 4) but says nothing of SORT, which RFC 5256 §4 answers with a SORT
// response. RFC 5267 §3.1 uses ESEARCH only for a SORT with return options.
func TestSortRev2PlainSortKeepsSortResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   imap.CapSet
		enable bool
	}{
		{"dual stack, ENABLE IMAP4rev2", dualStackCaps(), true},
		{"rev2-only server", imap.CapSet{imap.CapIMAP4rev2: {}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, tc.caps)
			rc.appendOne("one")
			rc.appendOne("two")
			if tc.enable {
				rc.do("ENABLE IMAP4rev2")
			}
			rc.do("SELECT INBOX")

			resp := rc.do("SORT (REVERSE ARRIVAL) UTF-8 ALL")
			if strings.Contains(resp, "* ESEARCH") {
				t.Errorf("plain SORT answered ESEARCH:\n%s", resp)
			}
			if !strings.Contains(resp, "* SORT 2 1\r\n") {
				t.Errorf("plain SORT did not answer * SORT 2 1:\n%s", resp)
			}

			resp = rc.do("SORT RETURN (ALL) (REVERSE ARRIVAL) UTF-8 ALL")
			if !strings.Contains(resp, "* ESEARCH (TAG ") || !strings.Contains(resp, " ALL 2,1") {
				t.Errorf("extended SORT did not answer ESEARCH ALL 2,1:\n%s", resp)
			}
		})
	}
}
