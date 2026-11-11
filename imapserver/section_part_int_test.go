package imapserver_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// A section part is read as a uint32 (RFC 9051 §9: nz-number) and kept in an
// int. On a 32-bit target an int cannot hold one above math.MaxInt32, and the
// conversion wrapped it to a negative part, echoed back as such. Such a part
// is refused there instead; where int is wider it is served. Issue #179.
func TestFetchSectionPartBeyondInt(t *testing.T) {
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
		max  uint64 // the largest part number in item
	}{
		{"BODY[2147483647]", math.MaxInt32},
		{"BODY[2147483648]", math.MaxInt32 + 1},
		{"BODY[4294967295]", math.MaxUint32},
		{"BODY.PEEK[1.4294967295]", math.MaxUint32},
		{"BODY[4294967295.HEADER]", math.MaxUint32},
		{"BINARY[2147483647]", math.MaxInt32},
		{"BINARY[4294967295]", math.MaxUint32},
		{"BINARY.PEEK[1.2147483648]", math.MaxInt32 + 1},
		{"BINARY.SIZE[4294967295]", math.MaxUint32},
	} {
		tag := fmt.Sprintf("b%d", i)
		fmt.Fprintf(w.conn, "%s FETCH 1 (%s)\r\n", tag, tc.item)
		var resp []string
		for {
			l := w.line()
			resp = append(resp, l)
			if strings.HasPrefix(l, tag+" ") {
				break
			}
		}
		got := resp[len(resp)-1]
		want := tag + " OK"
		if tc.max > math.MaxInt {
			want = tag + " BAD"
		}
		if !strings.HasPrefix(got, want) {
			t.Errorf("FETCH 1 (%s) answered %q, want %s", tc.item, got, want[len(tag)+1:])
		}
		for _, l := range resp[:len(resp)-1] {
			if strings.Contains(l, "[-") || strings.Contains(l, ".-") {
				t.Errorf("FETCH 1 (%s) echoed a negative part: %q", tc.item, l)
			}
		}
	}
}
