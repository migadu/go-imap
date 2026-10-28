package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
)

// The ESORT search-correlator carries the tag as a tag-string. RFC 4466 §3
// defines that as a string, which an atom is not; RFC 9051 widens it to an
// astring. A quoted tag satisfies both, so it is sent the way ESEARCH's is.
func TestESortTagIsQuoted(t *testing.T) {
	w := dialParseServer(t, nil)
	fmt.Fprint(w.conn, "a2 SELECT INBOX\r\n")
	w.until("a2")

	fmt.Fprint(w.conn, "a3 SORT RETURN (COUNT) (ARRIVAL) UTF-8 ALL\r\n")
	var esort string
	for {
		l := w.line()
		if strings.HasPrefix(l, "* ESORT") {
			esort = l
		}
		if strings.HasPrefix(l, "a3 ") {
			if !strings.HasPrefix(l, "a3 OK") {
				t.Fatalf("SORT answered %q; want OK", l)
			}
			break
		}
	}
	if want := `* ESORT (TAG "a3")`; !strings.HasPrefix(esort, want) {
		t.Errorf("ESORT response = %q; want prefix %q", esort, want)
	}
}
