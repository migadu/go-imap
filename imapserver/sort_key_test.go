package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// A sort key outside the grammar is invalid arguments, "BAD - command unknown
// or arguments invalid" (RFC 5256 §3):
//
//	sort-criterion  = ["REVERSE" SP] sort-key
//	sort-key        = "ARRIVAL" / "CC" / "DATE" / "FROM" / "SIZE" /
//	                  "SUBJECT" / "TO"
//
// (RFC 5256 §5) and sort-key =/ "DISPLAYFROM" / "DISPLAYTO" (RFC 5957 §5).
// It was answered "NO [SERVERBUG] Internal server error".
func TestSortUnknownKeyIsBAD(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapSort:      {},
		imap.CapESort:     {},
	})
	rc.appendOne("sort keys")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		`SORT (FOO) UTF-8 ALL`,
		`SORT (foo) UTF-8 ALL`,
		`SORT (REVERSE FOO) UTF-8 ALL`,
		`SORT (REVERSE REVERSE DATE) UTF-8 ALL`,
		`SORT (DATE FOO) UTF-8 ALL`,
		`UID SORT (FOO) UTF-8 ALL`,
		`SORT RETURN (ALL) (FOO) UTF-8 ALL`,
	} {
		resp, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
			t.Errorf("%s → %s, want BAD [CLIENTBUG]", cmd, strings.TrimRight(resp, "\r\n"))
		} else if resp != tagged {
			t.Errorf("%s: want no untagged response, got:\n%s", cmd, resp)
		}
		rc.requireInSync(cmd)
	}

	// Every key the grammar has is served, with and without REVERSE and in
	// either case.
	for _, key := range []string{
		"ARRIVAL", "CC", "DATE", "FROM", "SIZE", "SUBJECT", "TO",
		"DISPLAYFROM", "DISPLAYTO", "reverse date", "REVERSE subject",
	} {
		cmd := "SORT (" + key + ") UTF-8 ALL"
		if resp := rc.do(cmd); !strings.Contains(resp, "* SORT 1\r\n") {
			t.Errorf("%s: want * SORT 1, got:\n%s", cmd, resp)
		}
	}
}
