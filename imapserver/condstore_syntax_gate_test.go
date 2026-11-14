package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// Besides FETCH and STORE, CONDSTORE "adds a new MODSEQ search criterion",
// "defines an additional CONDSTORE parameter to SELECT/EXAMINE commands" and
// "adds the HIGHESTMODSEQ status data item to the STATUS command" (RFC 7162
// §3.1 e, h, i). Without CONDSTORE they were parsed and dropped: SEARCH MODSEQ
// matched every message whatever the mod-sequence, and SELECT (CONDSTORE) and
// STATUS (HIGHESTMODSEQ) were answered OK. RFC 4466 §2.1: "Any parameter not
// defined by extensions that the server supports must be rejected with a BAD
// response", and SELECT, STATUS and SEARCH answer invalid arguments with "BAD -
// command unknown or arguments invalid" (RFC 9051 §6.3.2, §6.3.11, §6.4.4).
// LIST-STATUS reads the same status data items (RFC 5819). IMAP4rev2 does not
// imply CONDSTORE (RFC 9051 Appendix F).
func TestCondStoreSyntaxRefusedWithoutCondStore(t *testing.T) {
	const bad = " BAD [CLIENTBUG] "
	// Sent in the authenticated state.
	authCmds := []string{
		`STATUS INBOX (HIGHESTMODSEQ)`,
		`STATUS INBOX (MESSAGES HIGHESTMODSEQ)`,
		`LIST "" "*" RETURN (STATUS (MESSAGES HIGHESTMODSEQ))`,
		`SELECT INBOX (CONDSTORE)`,
		`EXAMINE INBOX (CONDSTORE)`,
	}
	// Sent with INBOX selected. The one message's mod-sequence is far below
	// 99999999, so none of these may match it.
	selectedCmds := []string{
		`SEARCH MODSEQ 99999999`,
		`UID SEARCH MODSEQ 99999999`,
		`SEARCH MODSEQ "/flags/\\Seen" all 99999999`,
		`SEARCH OR MODSEQ 99999999 SEEN`,
		`SEARCH RETURN (COUNT) MODSEQ 99999999`,
		`SORT (ARRIVAL) UTF-8 MODSEQ 99999999`,
	}
	base := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}, imap.CapESearch: {}, imap.CapListStatus: {}}
	with := func(set imap.CapSet, caps ...imap.Cap) imap.CapSet {
		out := imap.CapSet{}
		for c := range set {
			out[c] = struct{}{}
		}
		for _, c := range caps {
			out[c] = struct{}{}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet // non-nil: reported through SessionCapabilities
		served  bool
	}{
		{
			name: "server without CONDSTORE",
			caps: base,
		},
		{
			name:    "session filtered CONDSTORE out",
			caps:    with(base, imap.CapCondStore),
			session: base,
		},
		{
			name: "rev2-only server",
			caps: imap.CapSet{imap.CapIMAP4rev2: {}, imap.CapSort: {}},
		},
		{
			name:   "server with CONDSTORE",
			caps:   with(base, imap.CapCondStore),
			served: true,
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
			rc.appendOne("condstore syntax gate")

			check := func(cmd string, served func(resp string) bool) {
				t.Helper()
				resp, tagged := rc.try(cmd)
				if !tc.served {
					// Refused before the session sees it: nothing but the
					// tagged BAD, and no mailbox selected by SELECT/EXAMINE.
					if !strings.Contains(tagged, bad) || resp != tagged {
						t.Errorf("%s: want only a tagged %q, got:\n%s", cmd, strings.TrimSpace(bad), resp)
					}
					return
				}
				if !strings.Contains(tagged, " OK ") || !served(resp) {
					t.Errorf("%s: want it answered, got:\n%s", cmd, resp)
				}
			}

			for _, cmd := range authCmds {
				check(cmd, func(resp string) bool {
					return strings.Contains(resp, "HIGHESTMODSEQ ")
				})
			}

			rc.do("SELECT INBOX")
			for _, cmd := range selectedCmds {
				check(cmd, func(resp string) bool {
					return !strings.Contains(resp, "* SEARCH 1") &&
						!strings.Contains(resp, "* SORT 1") &&
						!strings.Contains(resp, "COUNT 1")
				})
			}
			// The connection is still usable after a refusal.
			rc.do("NOOP")
		})
	}
}
