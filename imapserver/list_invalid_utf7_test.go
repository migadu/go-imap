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
