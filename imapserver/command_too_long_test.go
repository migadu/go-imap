package imapserver_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// A command longer than the server reads is the client's error: RFC 9051
// §7.1.3 has BAD report "a protocol-level error in the client's command", and
// §7.1.5 shows "BAD Command line too long". It used to be answered
// NO [SERVERBUG] and logged as a failure of the handler.
//
// The limit itself is the one upstream asks for in emersion/go-imap#574.
// Issue #133.
func TestCommandTooLongIsBad(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	rc.do("SELECT INBOX")

	// RFC 2683 §3.2.1.5 has servers accept command lines of 8000 octets;
	// this one is read.
	rc.do("SEARCH SUBJECT " + strings.Repeat("x", 8000))

	_, tagged := rc.try("SEARCH SUBJECT " + strings.Repeat("x", 80*1024))
	if !strings.Contains(tagged, " BAD ") || strings.Contains(tagged, "SERVERBUG") {
		t.Errorf("an 80 KiB command → %q, want a tagged BAD", tagged)
	}

	// The rest of the line was not read, so the connection ends.
	var rest bytes.Buffer
	io.Copy(&rest, rc.br)
	if !strings.Contains(rest.String(), "* BYE ") {
		t.Errorf("after the oversized command the server sent %q, want BYE", rest.String())
	}
}
