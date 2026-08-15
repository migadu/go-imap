package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadListMailboxUTF8NoUnescaping covers the LIST mailbox pattern, which
// has its own decode path separate from Decoder.ExpectMailbox.
//
// The rule is the same: with UTF-8 mode negotiated, modified UTF-7 does not
// apply (RFC 9051 Appendix E, RFC 6855 §3), so '&' is an ordinary character and
// the pattern is taken verbatim. Unescaping here would turn a client's LIST of
// "A&-B" -- four literal characters -- into a LIST of "A&B", quietly matching
// the wrong mailboxes.
func TestReadListMailboxUTF8NoUnescaping(t *testing.T) {
	for _, tc := range []struct {
		wire string
		want string
	}{
		{`"R&D"`, "R&D"},
		{`"A&-B"`, "A&-B"},
		{`"Sales & Marketing"`, "Sales & Marketing"},
		{`"&"`, "&"},
		// Wildcards must survive untouched alongside '&'.
		{`"R&D/*"`, "R&D/*"},
		{`"plain"`, "plain"},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.wire+"\r\n")), imapwire.ConnSideServer)
			dec.QuotedUTF8 = true

			got, err := readListMailbox(dec)
			if err != nil {
				t.Fatalf("readListMailbox(%s) = error %v", tc.wire, err)
			}
			if got != tc.want {
				t.Errorf("readListMailbox(%s) = %q, want %q", tc.wire, got, tc.want)
			}
		})
	}
}

// TestReadListMailboxUTF7ModeStillDecodes is the counterpart guard: with UTF-8
// mode off we are speaking IMAP4rev1, where the pattern really is modified
// UTF-7 and "&-" really does mean a literal '&'.
func TestReadListMailboxUTF7ModeStillDecodes(t *testing.T) {
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("\"R&-D\"\r\n")), imapwire.ConnSideServer)
	dec.QuotedUTF8 = false

	got, err := readListMailbox(dec)
	if err != nil {
		t.Fatalf("readListMailbox() = error %v", err)
	}
	if got != "R&D" {
		t.Errorf("readListMailbox(%q) = %q, want %q", "R&-D", got, "R&D")
	}
}
