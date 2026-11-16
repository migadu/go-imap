package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// ENABLE takes at least one capability name:
//
//	enable          = "ENABLE" 1*(SP capability)
//
// (RFC 9051 §9; command-any =/ "ENABLE" 1*(SP capability) in RFC 5161 §4), and
// a bare ENABLE is "BAD -  No arguments, or syntax error in an argument" (RFC
// 9051 §6.3.1, RFC 5161 §3.1). It was answered "* ENABLED" and OK.
func TestEnableWithoutCapabilityIsBAD(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapCondStore: {},
	})

	for _, cmd := range []string{
		`ENABLE`,
		`ENABLE `,
	} {
		resp, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
			t.Errorf("%q → %s, want BAD [CLIENTBUG]", cmd, strings.TrimRight(resp, "\r\n"))
		} else if resp != tagged {
			t.Errorf("%q: want no untagged response, got:\n%s", cmd, resp)
		}
		rc.requireInSync(cmd)
	}

	// With a capability ENABLE is served, also when the server does not know
	// it: "the server MUST ignore the argument" (RFC 9051 §6.3.1).
	for _, tc := range []struct{ cmd, want string }{
		{`ENABLE X-UNKNOWN`, "* ENABLED\r\n"},
		{`ENABLE CONDSTORE`, "* ENABLED CONDSTORE\r\n"},
		{`ENABLE X-UNKNOWN CONDSTORE`, "* ENABLED CONDSTORE\r\n"},
	} {
		if resp := rc.do(tc.cmd); !strings.Contains(resp, tc.want) {
			t.Errorf("%s: want %q, got:\n%s", tc.cmd, tc.want, resp)
		}
	}
}
