package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// RFC 4466 §2.2: "Any parameter not defined by extensions that the server
// supports must be rejected with a BAD response." The USE parameter is
// CREATE-SPECIAL-USE's (RFC 6154 §3: "Clients MUST NOT use the "USE" parameter
// unless the server advertises the capability"), with an empty attribute list
// too (RFC 6154 §6: "USE" SP "(" [use-attr *(SP use-attr)] ")"). The session
// cannot refuse "USE ()" itself: it reaches it as no USE parameter at all.
// IMAP4rev2 folds in only the special-use attributes (RFC 9051 Appendix E), not
// CREATE-SPECIAL-USE.
func TestCreateUseRefusedWithoutCapability(t *testing.T) {
	const bad = " BAD [CLIENTBUG] "
	refused := map[string]string{
		`CREATE Plain`:                   "",
		`CREATE EmptyUse (USE ())`:       bad,
		`CREATE Drafts2 (USE (\Drafts))`: bad,
	}
	served := map[string]string{
		`CREATE Plain`:                   "",
		`CREATE EmptyUse (USE ())`:       "",
		`CREATE Drafts2 (USE (\Drafts))`: "",
	}
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet // non-nil: reported through SessionCapabilities
		cmds    map[string]string
	}{
		{
			name: "server without CREATE-SPECIAL-USE",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}},
			cmds: refused,
		},
		{
			name:    "session filtered CREATE-SPECIAL-USE out",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCreateSpecialUse: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}},
			cmds:    refused,
		},
		{
			name: "rev2-only server",
			caps: imap.CapSet{imap.CapIMAP4rev2: {}},
			cmds: refused,
		},
		{
			name: "server with CREATE-SPECIAL-USE",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCreateSpecialUse: {}},
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
				name := strings.Fields(cmd)[1]
				listed := strings.Contains(rc.do(`LIST "" "`+name+`"`), "* LIST ")
				if want == bad {
					if !strings.Contains(tagged, bad) {
						t.Errorf("%s: want %q, got:\n%s", cmd, strings.TrimSpace(bad), resp)
					}
					if listed {
						t.Errorf("%s: refused, but created %s", cmd, name)
					}
				} else if !strings.Contains(tagged, " OK ") || !listed {
					t.Errorf("%s: want OK and %s created, got:\n%s", cmd, name, resp)
				}
			}
		})
	}
}
