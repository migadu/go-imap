package internal

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// The 17 keywords of RFC 9979 section 1, spelled as the "IMAP and JMAP
// Keywords" registry spells them.
var rfc9979Keywords = map[imap.Flag]string{
	imap.FlagAutoSent:        "$autosent",
	imap.FlagCanUnsubscribe:  "$canunsubscribe",
	imap.FlagFollowed:        "$followed",
	imap.FlagHasAttachment:   "$hasattachment",
	imap.FlagHasMemo:         "$hasmemo",
	imap.FlagHasNoAttachment: "$hasnoattachment",
	imap.FlagImported:        "$imported",
	imap.FlagIsTrusted:       "$istrusted",
	imap.FlagMailFlagBit0:    "$MailFlagBit0",
	imap.FlagMailFlagBit1:    "$MailFlagBit1",
	imap.FlagMailFlagBit2:    "$MailFlagBit2",
	imap.FlagMaskedEmail:     "$maskedemail",
	imap.FlagMemo:            "$memo",
	imap.FlagMuted:           "$muted",
	imap.FlagNew:             "$new",
	imap.FlagNotify:          "$notify",
	imap.FlagUnsubscribed:    "$unsubscribed",
}

func TestRFC9979Keywords(t *testing.T) {
	if len(rfc9979Keywords) != 17 {
		t.Fatalf("%d distinct keyword constants, want 17", len(rfc9979Keywords))
	}
	for flag, registered := range rfc9979Keywords {
		if string(flag) != registered {
			t.Errorf("constant %q, registered as %q", flag, registered)
		}
		// Keywords are matched case-insensitively, like the system flags.
		for _, wire := range []string{registered, strings.ToUpper(registered), strings.ToLower(registered)} {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(wire+" ")), imapwire.ConnSideServer)
			got, err := ExpectFlag(dec)
			if err != nil || got != flag {
				t.Errorf("ExpectFlag(%q) = %q, %v; want %q", wire, got, err, flag)
			}
		}
	}
}

// RFC 9979 section 9.2 registers Snoozed, Scheduled and Memos. IMAP has
// one syntax for a mailbox attribute: "\" atom, in LIST (RFC 9051
// mbx-list-oflag, flag-extension) and in CREATE USE (RFC 6154 use-attr-ext).
// The registry lists every name without its backslash (RFC 8457 section
// 6.2), and Cyrus sends \Snoozed and \Scheduled.
func TestRFC9979MailboxAttrs(t *testing.T) {
	for attr, name := range map[imap.MailboxAttr]string{
		imap.MailboxAttrSnoozed:   "Snoozed",
		imap.MailboxAttrScheduled: "Scheduled",
		imap.MailboxAttrMemos:     "Memos",
	} {
		t.Run(name, func(t *testing.T) {
			if want := imap.MailboxAttr(`\` + name); attr != want {
				t.Errorf("constant %q, want %q", attr, want)
			}

			var buf bytes.Buffer
			bw := bufio.NewWriter(&buf)
			enc := imapwire.NewEncoder(bw, imapwire.ConnSideServer)
			enc.Special('(').MailboxAttr(attr).Special(')')
			if err := enc.CRLF(); err != nil {
				t.Fatalf("encoding %q: %v", attr, err)
			}
			if want := "(\\" + name + ")\r\n"; buf.String() != want {
				t.Fatalf("encoded %q, want %q", buf.String(), want)
			}

			// Read back any case, and also the bare name that the section 9.2
			// note ("no implied backslash") would put on the wire.
			for _, wire := range []string{`\` + name, `\` + strings.ToUpper(name), `\` + strings.ToLower(name), name, strings.ToLower(name)} {
				dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("("+wire+")")), imapwire.ConnSideClient)
				got, err := ExpectMailboxAttrList(dec)
				if err != nil || len(got) != 1 || got[0] != attr {
					t.Errorf("ExpectMailboxAttrList(%q) = %q, %v; want [%q]", wire, got, err, attr)
				}
			}
		})
	}
}
