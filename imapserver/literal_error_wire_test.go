package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A literal the decoder refuses on sight -- a size beyond what the grammar's
// sanity bound allows -- is the client's syntax, so the answer is BAD rather
// than NO [SERVERBUG]. The announcement is rejected before any continuation
// request, so a synchronizing literal never has a payload to skip.
func TestOversizedLiteralAnnouncementIsBad(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{"beyond the sanity bound", `a2 SELECT {99999999999}`},
		{"does not fit in int64", `a2 SELECT {99999999999999999999999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialParseServer(t, nil)
			fmt.Fprint(w.conn, tc.cmd+"\r\n")
			got := w.until("a2")
			if strings.Contains(got, "SERVERBUG") {
				t.Errorf("oversized literal answered %q; want BAD", got)
			}
			if !strings.HasPrefix(got, "a2 BAD") {
				t.Errorf("answer was %q; want BAD", got)
			}
		})
	}
}

// The server's own size check still decides the response for a literal that is
// merely larger than it accepts: that stays NO [TOOBIG], the typed error the
// check returns, and is not folded into the syntax-error path.
func TestBufferedLiteralOverServerLimitIsTooBig(t *testing.T) {
	w := dialParseServer(t, nil)
	fmt.Fprint(w.conn, "a2 SELECT {5000}\r\n")
	got := w.until("a2")
	if !strings.HasPrefix(got, "a2 NO ") || !strings.Contains(got, "[TOOBIG]") {
		t.Errorf("answer was %q; want NO [TOOBIG]", got)
	}
}

// The non-synchronizing form of the same announcement has an unvalidated
// quantity of octets in flight behind it. The server cannot skip them, so it
// ends the connection with BYE rather than parse the next command from inside
// client data (RFC 9051 §2.2.1).
func TestOversizedNonSyncLiteralClosesConnection(t *testing.T) {
	w := dialParseServer(t, nil)
	fmt.Fprint(w.conn, "a2 SELECT {99999999999+}\r\n")
	for {
		_ = w.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		l, err := w.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("connection ended without a BYE: %v", err)
		}
		l = strings.TrimRight(l, "\r\n")
		if strings.HasPrefix(l, "* BYE") {
			return
		}
		if strings.HasPrefix(l, "a2 ") {
			t.Fatalf("got a tagged answer %q; want the connection closed with BYE", l)
		}
	}
}
