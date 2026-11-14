package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// CONDSTORE adds the CHANGEDSINCE FETCH modifier and the MODSEQ FETCH data
// item (RFC 7162 §3.1.4). RFC 4466 §2.4: "Any modifiers not defined by
// extensions that the server supports must be rejected with a BAD response."
// Without CONDSTORE, MODSEQ is not a fetch-att either, and FETCH answers
// invalid arguments with "BAD - command unknown or arguments invalid"
// (RFC 9051 §6.4.5). Dropping them instead served every message CHANGEDSINCE
// was meant to leave out, and answered a lone MODSEQ with "* 1 FETCH ()",
// which msg-att does not allow (RFC 9051 §9: at least one item). IMAP4rev2
// does not imply CONDSTORE (RFC 9051 Appendix F).
func TestFetchCondStoreItemsRefusedWithoutCondStore(t *testing.T) {
	const bad = " BAD [CLIENTBUG] "
	cmds := []string{
		`FETCH 1 MODSEQ`,
		`FETCH 1 (MODSEQ)`,
		`FETCH 1 (FLAGS MODSEQ)`,
		`UID FETCH 1 (MODSEQ)`,
		`FETCH 1 (FLAGS) (CHANGEDSINCE 1)`,
		`UID FETCH 1 (FLAGS) (CHANGEDSINCE 1)`,
		`FETCH 1 FLAGS CHANGEDSINCE 1`,
	}
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet // non-nil: reported through SessionCapabilities
		served  bool
	}{
		{
			name: "server without CONDSTORE",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}},
		},
		{
			name:    "session filtered CONDSTORE out",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}},
		},
		{
			name: "rev2-only server",
			caps: imap.CapSet{imap.CapIMAP4rev2: {}},
		},
		{
			name:   "server with CONDSTORE",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}},
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
			rc.appendOne("condstore gate")
			rc.do("SELECT INBOX")
			for _, cmd := range cmds {
				resp, tagged := rc.try(cmd)
				if !tc.served {
					if !strings.Contains(tagged, bad) || strings.Contains(resp, " FETCH ") {
						t.Errorf("%s: want %q and no FETCH response, got:\n%s", cmd, strings.TrimSpace(bad), resp)
					}
					continue
				}
				if !strings.Contains(tagged, " OK ") {
					t.Errorf("%s: want OK, got:\n%s", cmd, resp)
				} else if strings.Contains(cmd, "MODSEQ") && !strings.Contains(resp, "MODSEQ (") {
					t.Errorf("%s: want a MODSEQ item, got:\n%s", cmd, resp)
				}
			}
			// The connection is still usable after a refusal.
			rc.do("NOOP")
		})
	}
}

// NOTIFY's MessageNew fetch-atts are FETCH's (RFC 5465 §5.2), and NOTIFY
// answers unsupported arguments with "BAD - Command unknown, invalid,
// unsupported, or has unknown arguments" (RFC 5465 §3.1). MODSEQ used to be
// accepted and dropped there too.
func TestNotifyModSeqRefusedWithoutCondStore(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t) // no CONDSTORE
	defer closer()
	c := dialNotifyTest(t, addr)
	defer c.close()
	c.login(t)
	const cmd = "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge))"
	if resp := c.cmd(cmd); !strings.Contains(resp, " BAD [CLIENTBUG] ") {
		t.Errorf("%s: want BAD [CLIENTBUG], got:\n%s", cmd, resp)
	}
	if resp := c.cmd("NOOP"); !strings.Contains(resp, " OK ") {
		t.Errorf("NOOP after refused NOTIFY: %q", resp)
	}
}
