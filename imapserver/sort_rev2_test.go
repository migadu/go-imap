package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
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
		// IMAP4rev2 does not include ESORT, which the extended SORT below needs.
		{"dual stack, ENABLE IMAP4rev2", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapESort: {}}, true},
		{"rev2-only server", imap.CapSet{imap.CapIMAP4rev2: {}, imap.CapESort: {}}, false},
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

// RFC 5267 §3 / §4.2 / RFC 4731 §3.1 / RFC 4466 §2.6.2:
// Extended SORT returns its results in an ESEARCH response (there is no ESORT
// response token in any RFC; ESORT is only the capability name in §5), and
// search-correlator = "(" "TAG" SP tag-string ")" where tag-string = string.
func TestESortTagCorrelatorIsString(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapSort:      {},
		imap.CapESort:     {},
	})
	rc.appendOne("one")
	rc.appendOne("two")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		"UID SORT RETURN (MIN MAX COUNT ALL) (ARRIVAL) UTF-8 ALL",
		"SORT RETURN (MIN MAX COUNT ALL) (ARRIVAL) UTF-8 ALL",
	} {
		resp := rc.do(cmd)
		if strings.Contains(resp, "* ESORT") {
			t.Fatalf("%s emitted non-standard * ESORT response (RFC 5267 §3 requires * ESEARCH):\n%s", cmd, resp)
		}
		var esearchLine string
		for _, line := range strings.Split(resp, "\r\n") {
			if strings.HasPrefix(line, "* ESEARCH ") {
				esearchLine = line
				break
			}
		}
		if esearchLine == "" {
			t.Fatalf("%s: expected * ESEARCH response line, got:\n%s", cmd, resp)
		}
		if !strings.Contains(esearchLine, `(TAG "t`) {
			t.Fatalf("%s: ESEARCH TAG correlator must be a quoted string (RFC 4466 §2.6.2), got: %q", cmd, esearchLine)
		}
	}
}

// RFC 9051 §6.4.4 deprecates only the SEARCH response; it does not touch SORT
// (RFC 5256). A client that has ENABLEd IMAP4rev2 and sends a plain UID SORT
// without a RETURN clause must still receive * SORT, never * ESEARCH or * ESORT.
// TestSortRev2PlainSortKeepsSortResponse covers SORT itself.
func TestSortRev2PlainUIDSortKeepsSortResponse(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIMAP4rev2: {},
		imap.CapSort:      {},
		imap.CapESort:     {},
	})
	rc.appendOne("one")
	rc.appendOne("two")
	rc.do("ENABLE IMAP4rev2")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		"UID SORT (ARRIVAL) UTF-8 ALL",
	} {
		resp := rc.do(cmd)
		if strings.Contains(resp, "* ESEARCH") || strings.Contains(resp, "* ESORT") {
			t.Fatalf("rev2 plain %s answered extended form instead of * SORT:\n%s", cmd, resp)
		}
		if !strings.Contains(resp, "* SORT 1 2") {
			t.Fatalf("rev2 plain %s did not answer * SORT 1 2:\n%s", cmd, resp)
		}
	}
}
