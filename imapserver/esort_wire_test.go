package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
)

// An extended SORT is answered with ESEARCH (RFC 5267 §3.1); no RFC defines an
// ESORT response. The search-correlator carries the tag as a tag-string. RFC
// 4466 §3 defines that as a string, which an atom is not; RFC 9051 widens it to
// an astring. A quoted tag satisfies both.
func TestExtendedSortAnswersESearch(t *testing.T) {
	w := dialParseServer(t, nil)
	fmt.Fprint(w.conn, "a2 SELECT INBOX\r\n")
	w.until("a2")

	fmt.Fprint(w.conn, "a3 SORT RETURN (COUNT) (ARRIVAL) UTF-8 ALL\r\n")
	var esearch string
	for {
		l := w.line()
		if strings.HasPrefix(l, "* ESORT") {
			t.Errorf("got %q; want an ESEARCH response", l)
		}
		if strings.HasPrefix(l, "* ESEARCH") {
			esearch = l
		}
		if strings.HasPrefix(l, "a3 ") {
			if !strings.HasPrefix(l, "a3 OK") {
				t.Fatalf("SORT answered %q; want OK", l)
			}
			break
		}
	}
	if want := `* ESEARCH (TAG "a3") COUNT 0`; esearch != want {
		t.Errorf("SORT response = %q; want %q", esearch, want)
	}
}
