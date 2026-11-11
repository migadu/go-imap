package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
)

// RFC 9051 §9: section-part = nz-number *("." nz-number). A part number too
// large for 32 bits, or zero, makes the command malformed. The decoder read
// the digits of an oversized number and reported no number at all, so the
// section went on without its part: BODY[4294967296] was served as BODY[],
// BODY[4294967296HEADER] as BODY[HEADER]. Issue #168.
func TestFetchSectionPartOutOfRange(t *testing.T) {
	w := dialParseServer(t, nil)

	msg := "Subject: hi\r\n\r\nbody\r\n"
	fmt.Fprintf(w.conn, "a2 APPEND INBOX {%d}\r\n", len(msg))
	if l := w.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("APPEND continuation: got %q", l)
	}
	fmt.Fprint(w.conn, msg+"\r\n")
	if got := w.until("a2"); !strings.HasPrefix(got, "a2 OK") {
		t.Fatalf("APPEND: %q", got)
	}
	fmt.Fprint(w.conn, "a3 SELECT INBOX\r\n")
	if got := w.until("a3"); !strings.HasPrefix(got, "a3 OK") {
		t.Fatalf("SELECT: %q", got)
	}

	for i, tc := range []struct {
		item string
		ok   bool
	}{
		{"BODY[4294967296]", false},
		{"BODY.PEEK[4294967296]", false},
		{"BODY[4294967296HEADER]", false},
		{"BODY[1.4294967296]", false},
		{"BODY[0]", false},
		{"BODY[1.0]", false},
		{"BODY[0.HEADER]", false},
		{"BINARY[4294967296]", false},
		{"BINARY[0]", false},
		{"BINARY.SIZE[0]", false},
		{"BODY[4294967295]", true},
		{"BODY[1]", true},
		{"BODY[1.1.MIME]", true},
		{"BINARY[1]", true},
	} {
		tag := fmt.Sprintf("b%d", i)
		fmt.Fprintf(w.conn, "%s FETCH 1 (%s)\r\n", tag, tc.item)
		got := w.until(tag)
		want := tag + " BAD"
		if tc.ok {
			want = tag + " OK"
		}
		if !strings.HasPrefix(got, want) {
			t.Errorf("FETCH 1 (%s) answered %q, want %s", tc.item, got, want[len(tag)+1:])
		}
	}
}
