package imapserver_test

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// filteredCapsSession reports narrower capabilities than the server's, as a
// backend that filters capabilities per client does.
type filteredCapsSession struct {
	imapserver.Session
	caps imap.CapSet
}

func (s *filteredCapsSession) GetCapabilities() imap.CapSet { return s.caps }

// RFC 4466 §2.6.1: "Any options not defined by extensions that the server
// supports must be rejected with a BAD response." The SEARCH result options are
// ESEARCH's, SAVE included (RFC 5182 §2.1), and the RETURN after SORT is ESORT's
// (RFC 5267 §3). The session cannot refuse them itself: a plain SEARCH or SORT
// reaches it as RETURN (ALL).
func TestReturnRefusedWithoutESearchOrESort(t *testing.T) {
	const bad = " BAD [CLIENTBUG] "
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet // non-nil: reported through SessionCapabilities
		cmds    map[string]string
	}{
		{
			name: "server without ESEARCH or ESORT",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}},
			cmds: map[string]string{
				"SEARCH ALL":                                  "* SEARCH 1 2\r\n",
				"SEARCH RETURN () ALL":                        "* SEARCH 1 2\r\n", // no option to refuse
				"SEARCH RETURN (ALL) ALL":                     bad,
				"UID SEARCH RETURN (ALL) ALL":                 bad,
				"SEARCH RETURN (MIN) ALL":                     bad,
				"SEARCH RETURN (MAX) ALL":                     bad,
				"SEARCH RETURN (COUNT) ALL":                   bad,
				"SEARCH RETURN (SAVE) ALL":                    bad,
				"SORT (ARRIVAL) UTF-8 ALL":                    "* SORT 1 2\r\n",
				"SORT RETURN () (ARRIVAL) UTF-8 ALL":          bad,
				"SORT RETURN (ALL) (ARRIVAL) UTF-8 ALL":       bad,
				"UID SORT RETURN (COUNT) (ARRIVAL) UTF-8 ALL": bad,
			},
		},
		{
			name:    "session filtered ESEARCH and ESORT out",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSort: {}, imap.CapESort: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}},
			cmds: map[string]string{
				"SEARCH ALL":                              "* SEARCH 1 2\r\n",
				"SEARCH RETURN (ALL) ALL":                 bad,
				"SEARCH RETURN (COUNT) ALL":               bad,
				"SORT (ARRIVAL) UTF-8 ALL":                "* SORT 1 2\r\n",
				"SORT RETURN (COUNT) (ARRIVAL) UTF-8 ALL": bad,
			},
		},
		{
			name: "server with ESEARCH and ESORT",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSort: {}, imap.CapESort: {}},
			cmds: map[string]string{
				"SEARCH RETURN (ALL) ALL":                 " ALL 1:2\r\n",
				"SEARCH RETURN (COUNT) ALL":               " COUNT 2\r\n",
				"SORT RETURN (COUNT) (ARRIVAL) UTF-8 ALL": " COUNT 2\r\n",
			},
		},
		{
			// IMAP4rev2 includes ESEARCH but not ESORT.
			name: "rev2-only server",
			caps: imap.CapSet{imap.CapIMAP4rev2: {}},
			cmds: map[string]string{
				"SEARCH RETURN (COUNT) ALL":             " COUNT 2\r\n",
				"SORT RETURN (ALL) (ARRIVAL) UTF-8 ALL": bad,
			},
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
			rc.appendOne("one")
			rc.appendOne("two")
			rc.do("SELECT INBOX")
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
