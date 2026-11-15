package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// A mailbox name or pattern that is not valid modified UTF-7 is the client's
// mistake wherever it stands: BAD "reports a protocol-level error in the
// client's command" (RFC 9051 §7.1.3). LIST and LSUB used to answer it with
// NO [SERVERBUG], and log it as a fault of the server.
//
// The name is the one of imaptest's "atoms" script, with its bare "&"
// (emersion/go-imap#243). Issue #149.
func TestListInvalidUTF7IsBad(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}})

	const atoms = "!#$$&'+,-.0123456789:;<=>?@^_`|[}"
	for _, cmd := range []string{
		"SELECT " + atoms,
		`LIST "" ` + atoms + "%*",
		"LIST " + atoms + ` "*"`,
		`LSUB "" ` + atoms + "%*",
		`LIST "" (` + atoms + "%*)",
		`LIST (SUBSCRIBED) "" ` + atoms + "%*",
	} {
		_, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " BAD ") || strings.Contains(tagged, "SERVERBUG") {
			t.Errorf("%s → %q, want BAD", cmd, strings.TrimSpace(tagged))
		}
	}

	// With the "&" written as modified UTF-7 has it, the name is fine.
	valid := strings.Replace(atoms, "&", "&-", 1)
	if resp, tagged := rc.try(`LIST "" ` + valid + "%*"); !strings.Contains(tagged, " OK ") {
		t.Errorf("LIST with a valid name → %q %q, want OK", resp, tagged)
	}
}

// Non-zero discarded BASE64 bits must not turn an invalid wire name into an
// alias of an existing mailbox, or create a mailbox LIST spells differently.
// Both mailbox arguments and LIST/LSUB patterns use the same UTF-7 decoder.
func TestMailboxUTF7NonZeroTrailingBitsIsBad(t *testing.T) {
	for _, tc := range []struct{ invalid, canonical string }{
		{"&AOl-", "&AOk-"},
		{"&AOn-", "&AOk-"},
		{"&AOkA6R-", "&AOkA6Q-"},
	} {
		t.Run(tc.invalid, func(t *testing.T) {
			rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListExtended: {}})
			bad := func(cmd string) {
				t.Helper()
				if _, tagged := rc.try(cmd); !strings.Contains(tagged, " BAD ") {
					t.Fatalf("%s → %q, want BAD", cmd, strings.TrimSpace(tagged))
				}
				rc.do("NOOP") // Refusing the name leaves the connection in sync.
			}
			bad(`CREATE "` + tc.invalid + `"`)
			if resp := rc.do(`LIST "" "` + tc.canonical + `"`); strings.Contains(resp, "* LIST ") {
				t.Fatalf("refused CREATE created the canonical mailbox: %q", resp)
			}
			rc.do(`CREATE "` + tc.canonical + `"`)
			rc.do(`SUBSCRIBE "` + tc.canonical + `"`)
			for _, cmd := range []string{
				`SELECT "` + tc.invalid + `"`,
				`LIST "" "` + tc.invalid + `"`,
				`LIST "` + tc.invalid + `" "*"`,
				`LIST "" ("` + tc.invalid + `")`,
				`LSUB "" "` + tc.invalid + `"`,
			} {
				bad(cmd)
			}
			rc.do(`SELECT "` + tc.canonical + `"`)
			if resp := rc.do(`LIST "" "` + tc.canonical + `"`); !strings.Contains(resp, `"`+tc.canonical+`"`) {
				t.Fatalf("LIST changed the canonical spelling: %q", resp)
			}

			// After UTF-8 negotiation, '&' is ordinary mailbox-name text.
			rc.do("ENABLE UTF8=ACCEPT")
			rc.do(`CREATE "` + tc.invalid + `"`)
			if resp := rc.do(`LIST "" "` + tc.invalid + `"`); !strings.Contains(resp, `"`+tc.invalid+`"`) {
				t.Fatalf("UTF-8 mailbox name was decoded as UTF-7: %q", resp)
			}
		})
	}
}
