package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// RFC 9051 §6.4.4: "When multiple keys are specified, the result is the
// intersection (AND function) of all the messages that match those keys." A
// SMALLER key therefore limits the result wherever it stands in the search
// program, including ahead of the date keys and LARGER, which the decoder
// merges into the criteria with SearchCriteria.And.
func TestSearchSmallerKeptBeforeLaterKeys(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	rc.appendOne("s")                      // UID 1, 20 octets
	rc.appendOne(strings.Repeat("x", 100)) // UID 2, 119 octets
	rc.do("SELECT INBOX")

	for _, tc := range []struct {
		criteria, want string
	}{
		{"SMALLER 50", "1"},
		{"SINCE 1-Jan-2020 SMALLER 50", "1"},
		{"SMALLER 50 SINCE 1-Jan-2020", "1"},
		{"SMALLER 50 BEFORE 1-Jan-2100", "1"},
		{"SMALLER 50 LARGER 10", "1"},
		{"SMALLER 50 SMALLER 200", "1"},
		{"(SMALLER 50 SINCE 1-Jan-2020)", "1"},
		{"NOT (SMALLER 50 SINCE 1-Jan-2020)", "2"},
		{"OR (SMALLER 50 SINCE 1-Jan-2020) UID 3", "1"},
	} {
		resp := rc.do("UID SEARCH " + tc.criteria)
		if want := "* SEARCH " + tc.want + "\r\n"; !strings.HasPrefix(resp, want) {
			t.Errorf("UID SEARCH %s: got %q, want %q", tc.criteria, resp, want)
		}
	}
}
