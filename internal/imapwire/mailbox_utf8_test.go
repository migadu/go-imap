package imapwire

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// Mailbox names that exercise '&', the one character modified UTF-7 gives a
// special meaning to (RFC 3501 §5.1.3: a literal '&' is sent as "&-").
//
// In UTF-8 mode that special meaning is gone. RFC 9051 Appendix E removes
// modified UTF-7 from IMAP4rev2 outright, and RFC 6855 §3 forbids it once
// UTF8=ACCEPT is enabled, so '&' is an ordinary character and the name goes on
// the wire verbatim.
//
// These are not exotic names: "R&D" and "Sales & Marketing" are the kind of
// folder people actually have.
var utf8MailboxNames = []struct {
	name string // the mailbox name the application asked for
	wire string // exactly what UTF-8 mode must put between the quotes
}{
	{"R&D", "R&D"},
	{"Sales & Marketing", "Sales & Marketing"},
	// A name that literally contains the modified-UTF-7 escape sequence. In
	// UTF-8 mode it is four ordinary characters and must survive untouched.
	{"A&-B", "A&-B"},
	{"&", "&"},
	{"plain", "plain"},
}

// TestEncodeMailboxUTF8NoUTF7Escaping checks that UTF-8 mode puts the mailbox
// name on the wire verbatim.
//
// Escaping '&' here corrupts the name for any conformant peer: a server reading
// IMAP4rev2 takes the bytes literally, so a CREATE of "R&D" that goes out as
// "R&-D" creates a mailbox actually named "R&-D".
func TestEncodeMailboxUTF8NoUTF7Escaping(t *testing.T) {
	for _, tc := range utf8MailboxNames {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			bw := bufio.NewWriter(&buf)
			enc := NewEncoder(bw, ConnSideClient)
			enc.QuotedUTF8 = true
			enc.Mailbox(tc.name)
			if err := bw.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}

			got := buf.String()
			// The encoder may pick a quoted string or an atom; compare the
			// payload rather than the framing.
			payload := strings.Trim(got, `"`)
			if payload != tc.wire {
				t.Errorf("Mailbox(%q) put %q on the wire, want %q", tc.name, payload, tc.wire)
			}
		})
	}
}

// TestDecodeMailboxUTF8NoUTF7Unescaping is the receiving half: in UTF-8 mode
// the bytes on the wire are the name, with no unescaping applied.
//
// Unescaping here corrupts names sent by conformant clients: a client that
// creates a mailbox genuinely called "A&-B" has that name silently folded to
// "A&B".
func TestDecodeMailboxUTF8NoUTF7Unescaping(t *testing.T) {
	for _, tc := range utf8MailboxNames {
		t.Run(tc.name, func(t *testing.T) {
			input := `"` + tc.wire + `"` + "\r\n"
			dec := NewDecoder(bufio.NewReader(strings.NewReader(input)), ConnSideServer)
			dec.QuotedUTF8 = true

			var got string
			if !dec.ExpectMailbox(&got) {
				t.Fatalf("ExpectMailbox() failed: %v", dec.Err())
			}
			if got != tc.name {
				t.Errorf("wire %q decoded to %q, want %q", tc.wire, got, tc.name)
			}
		})
	}
}

// TestMailboxUTF7ModeStillEscapes guards the other half of the switch: with
// UTF-8 mode off we are speaking IMAP4rev1, where modified UTF-7 very much does
// apply and a literal '&' must still be sent as "&-".
//
// Without this, "stop escaping in UTF-8 mode" could be satisfied by never
// escaping at all, which would break every IMAP4rev1 peer.
func TestMailboxUTF7ModeStillEscapes(t *testing.T) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	enc := NewEncoder(bw, ConnSideClient)
	enc.QuotedUTF8 = false
	enc.Mailbox("R&D")
	if err := bw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if payload := strings.Trim(buf.String(), `"`); payload != "R&-D" {
		t.Errorf("Mailbox(%q) in modified UTF-7 mode put %q on the wire, want %q", "R&D", payload, "R&-D")
	}

	dec := NewDecoder(bufio.NewReader(strings.NewReader("\"R&-D\"\r\n")), ConnSideServer)
	dec.QuotedUTF8 = false
	var got string
	if !dec.ExpectMailbox(&got) {
		t.Fatalf("ExpectMailbox() failed: %v", dec.Err())
	}
	if got != "R&D" {
		t.Errorf("modified UTF-7 wire %q decoded to %q, want %q", "R&-D", got, "R&D")
	}
}
