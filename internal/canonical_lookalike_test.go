package internal

import (
	"testing"

	"github.com/migadu/go-imap"
)

// Flags, mailbox attributes and capability names are matched without regard
// to ASCII case only (RFC 5234 §2.3: "ABNF strings are case insensitive and
// the character set for these strings is US-ASCII"). strings.ToLower maps
// U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE) to "i" and U+212A (KELVIN
// SIGN) to "k", and strings.ToUpper maps U+017F (LATIN SMALL LETTER LONG S)
// to "S", so a look-alike was canonicalized to the registered name.
func TestCanonicalNamesIgnoreLookalikes(t *testing.T) {
	const (
		dottedI = "İ"
		kelvin  = "K"
		longS   = "ſ"
	)
	for _, tc := range []struct{ in, want imap.Flag }{
		{"$" + dottedI + "mported", "$" + dottedI + "mported"},
		{"$" + dottedI + "STRUSTED", "$" + dottedI + "STRUSTED"},
		{"$IMPORTED", imap.FlagImported},
		{"\\seen", imap.FlagSeen},
	} {
		if got := canonicalFlag(string(tc.in)); got != tc.want {
			t.Errorf("canonicalFlag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want imap.MailboxAttr }{
		{"\\" + dottedI + "mportant", "\\" + dottedI + "mportant"},
		{"\\Jun" + kelvin, "\\Jun" + kelvin},
		{"\\IMPORTANT", imap.MailboxAttrImportant},
		{"\\junk", imap.MailboxAttrJunk},
	} {
		if got := canonicalMailboxAttr(string(tc.in)); got != tc.want {
			t.Errorf("canonicalMailboxAttr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want imap.Cap }{
		{"COND" + longS + "TORE", "COND" + longS + "TORE"},
		{"QRE" + longS + "YNC", "QRE" + longS + "YNC"},
		{"RIGHT" + longS + "=kx", "RIGHT" + longS + "=KX"}, // an unknown name, upper-cased whole
		{"condstore", imap.CapCondStore},
		{"imap4REV2", imap.CapIMAP4rev2},
		{"rights=kx", "RIGHTS=kx"},
	} {
		if got := canonicalCap(string(tc.in)); got != tc.want {
			t.Errorf("canonicalCap(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
