package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// RFC 5258 §3: "each option will be enabled by a capability string (one
// capability may enable multiple options), and a client MUST NOT send an option
// for which the server has not advertised support. A server MUST respond to
// options it does not recognize with a BAD response." The selection and return
// options, and the parenthesized pattern list, are LIST-EXTENDED syntax. The
// session cannot refuse them itself: LSUB reaches it as LIST (SUBSCRIBED
// RECURSIVEMATCH), and "()" or a one-pattern list arrives as a plain LIST.
func TestListExtendedRefusedWithoutCapability(t *testing.T) {
	const bad = " BAD [CLIENTBUG] "
	refused := map[string]string{
		`LIST "" "*"`:              "* LIST ",
		`LSUB "" "*"`:              "",
		`LIST (SUBSCRIBED) "" "*"`: bad,
		`LIST (SUBSCRIBED RECURSIVEMATCH) "" "*"`:  bad,
		`LIST (REMOTE) "" "*"`:                     bad,
		`LIST () "" "*"`:                           bad,
		`LIST "" ("*")`:                            bad,
		`LIST "" ("INBOX" "Archive")`:              bad,
		`LIST "" "*" RETURN (CHILDREN)`:            bad,
		`LIST "" "*" RETURN (SUBSCRIBED)`:          bad,
		`LIST "" "*" RETURN ()`:                    bad,
		`LIST "" "*" RETURN (STATUS (MESSAGES))`:   bad,
		`LIST (SPECIAL-USE) "" "*"`:                bad,
		`LIST "" "*" RETURN (SPECIAL-USE)`:         bad,
		`LIST (SUBSCRIBED) "" ("INBOX" "Archive")`: bad,
	}
	served := map[string]string{
		`LIST "" "*"`:              "* LIST ",
		`LSUB "" "*"`:              "",
		`LIST (SUBSCRIBED) "" "*"`: "",
		`LIST (SUBSCRIBED RECURSIVEMATCH) "" "*"`: "",
		`LIST () "" "*"`:                           "* LIST ",
		`LIST "" ("*")`:                            "* LIST ",
		`LIST "" ("INBOX" "Archive")`:              "* LIST ",
		`LIST "" "*" RETURN (CHILDREN)`:            "* LIST ",
		`LIST "" "*" RETURN (SUBSCRIBED)`:          "* LIST ",
		`LIST "" "*" RETURN ()`:                    "* LIST ",
		`LIST (SUBSCRIBED) "" ("INBOX" "Archive")`: "",
	}
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet // non-nil: reported through SessionCapabilities
		cmds    map[string]string
	}{
		{
			name: "server without LIST-EXTENDED",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}},
			cmds: refused,
		},
		{
			name:    "session filtered LIST-EXTENDED out",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListExtended: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}},
			cmds:    refused,
		},
		{
			name: "server with LIST-EXTENDED",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListExtended: {}},
			cmds: served,
		},
		{
			// IMAP4rev2 includes LIST-EXTENDED (RFC 9051 §6.3.9).
			name: "rev2-only server",
			caps: imap.CapSet{imap.CapIMAP4rev2: {}},
			cmds: served,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wrap func(imapserver.Session) imapserver.Session
			if tc.session != nil {
				wrap = func(s imapserver.Session) imapserver.Session {
					return &filteredCapsSession{s, tc.session}
				}
			}
			rc := newSearchRev2ConnWrapped(t, tc.caps, wrap)
			for cmd, want := range tc.cmds {
				resp, tagged := rc.try(cmd)
				var ok bool
				if want == bad {
					ok = strings.Contains(tagged, bad)
				} else {
					ok = strings.Contains(resp, want) && strings.Contains(tagged, " OK ")
				}
				if !ok {
					t.Errorf("%s: want %q, got:\n%s", cmd, strings.TrimSpace(want), resp)
				}
			}
		})
	}
}
