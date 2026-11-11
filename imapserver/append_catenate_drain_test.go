package imapserver_test

import (
	"io"
	"strings"
	"testing"
)

// The non-synchronizing literals of a CATENATE append are on the wire
// whatever the server answers (RFC 7888 §3), however the command fails: they
// are read before the next command, and the failure reported is the one that
// ended the command. Issue #165.

// Before authentication the command is refused, as a plain APPEND is (see
// handleAppend), but only once what the client sent of it has been read. No
// continuation request is sent for a synchronizing literal.
func TestAppendCatenateUnauthenticated(t *testing.T) {
	rc := dialCatenateConn(t, 1<<20)

	for _, cmd := range []string{
		"APPEND INBOX CATENATE (TEXT {5+}\r\nhello)",
		`APPEND INBOX CATENATE (URL "/INBOX/;UID=1/;SECTION=1" TEXT {3+}` + "\r\nabc TEXT {3+}\r\ndef)",
	} {
		if _, tagged := rc.try(cmd); !strings.Contains(tagged, " BAD ") {
			t.Errorf("%q before LOGIN → %q, want BAD", cmd, tagged)
		}
		rc.requireInSync("APPEND CATENATE before LOGIN")
	}

	tagged, contReqs := rc.exchange("APPEND INBOX CATENATE (TEXT {5}", "hello)")
	if !strings.Contains(tagged, " BAD ") || contReqs != 0 {
		t.Errorf("APPEND CATENATE before LOGIN → %q after %d continuation requests, want BAD after none", tagged, contReqs)
	}
	rc.requireInSync("APPEND CATENATE before LOGIN, synchronizing")

	rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)
	if resp := rc.do("STATUS INBOX (MESSAGES)"); !strings.Contains(resp, "MESSAGES 0") {
		t.Errorf("an APPEND before LOGIN stored a message: %q", resp)
	}
}

// RFC 4469 §4.2 allows TOOBIG for a message over the server's limit. The
// non-synchronizing parts after the one that went over are read too.
func TestAppendCatenateTooBigDrainsLaterParts(t *testing.T) {
	rc := newCatenateConn(t, 10)

	for _, cmd := range []string{
		"APPEND INBOX CATENATE (TEXT {6+}\r\nabcdef TEXT {6+}\r\nabcdef TEXT {3+}\r\nxyz)",
		"APPEND INBOX CATENATE (TEXT {12+}\r\n0123456789ab TEXT {3+}\r\nxyz)",
	} {
		if _, tagged := rc.try(cmd); !strings.Contains(tagged, " NO [TOOBIG] ") {
			t.Errorf("%q → %q, want NO [TOOBIG]", cmd, tagged)
		}
		rc.requireInSync("TEXT parts over the limit, then more")
	}

	// The client waits for a continuation request that does not come.
	tagged, contReqs := rc.exchange("APPEND INBOX CATENATE (TEXT {12+}\r\n0123456789ab TEXT {3}", "xyz)")
	if !strings.Contains(tagged, " NO [TOOBIG] ") || contReqs != 0 {
		t.Errorf("TEXT parts over the limit, then a synchronizing one → %q after %d continuation requests, want NO [TOOBIG] after none", tagged, contReqs)
	}
	rc.requireInSync("TEXT parts over the limit, then a synchronizing one")

	if resp := rc.do("STATUS INBOX (MESSAGES)"); !strings.Contains(resp, "MESSAGES 0") {
		t.Errorf("an APPEND over the limit stored a message: %q", resp)
	}
}

// RFC 4469 §4.1: a URL the server cannot process is answered NO [BADURL].
// That stays the answer when the TEXT parts the client sent after it would
// not have fitted the APPEND limit: they are read to resynchronize, not
// appended, and are bounded as DiscardLine bounds what it skips.
func TestAppendCatenateFailureKeptWhileDraining(t *testing.T) {
	rc := newCatenateConn(t, 10)

	for _, tc := range []struct{ cmd, want string }{
		{`APPEND INBOX CATENATE (URL "/bad" TEXT {12+}` + "\r\n0123456789ab)", " NO [BADURL /bad] "},
		{`APPEND INBOX CATENATE (URL "/bad" TEXT {12+}` + "\r\n0123456789ab TEXT {3+}\r\nxyz)", " NO [BADURL /bad] "},
		{"APPEND Missing CATENATE (TEXT {12+}\r\n0123456789ab TEXT {3+}\r\nxyz)", " NO [TRYCREATE] "},
	} {
		if _, tagged := rc.try(tc.cmd); !strings.Contains(tagged, tc.want) {
			t.Errorf("%q → %q, want%s", tc.cmd, tagged, tc.want)
		}
		rc.requireInSync("a failed CATENATE ahead of TEXT parts over the limit")
	}

	// Beyond what may be read for nothing, the connection ends instead.
	if _, err := rc.c.Write([]byte(`t99 APPEND INBOX CATENATE (URL "/bad" TEXT {70000+}` + "\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	rest, _ := io.ReadAll(rc.br)
	if !strings.HasPrefix(string(rest), "* BYE ") {
		t.Errorf("a failed CATENATE ahead of a 70000-octet literal → %q, want BYE", rest)
	}
}
