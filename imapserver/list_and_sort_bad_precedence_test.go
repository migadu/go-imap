package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestListEmptySelectOptsBeforeOversizedLiteralKeepsTooBig(t *testing.T) {
	t.Run("LITERAL+ valid LIST () with RETURN (STATUS) and 4097-byte pattern", func(t *testing.T) {
		var probe *listOptionSession
		caps := imap.CapSet{
			imap.CapIMAP4rev1:   {},
			imap.CapLiteralPlus: {},
			imap.CapListStatus:  {},
		}
		rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
			probe = &listOptionSession{Session: sess}
			return probe
		})
		rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

		// Control: 4096-byte pattern literal with the exact same syntax succeeds with OK.
		pat4096 := strings.Repeat("x", 4096)
		okWire := fmt.Sprintf("L0 LIST () \"\" {%d+}\r\n%s RETURN (STATUS (MESSAGES))\r\n", len(pat4096), pat4096)
		if _, err := fmt.Fprint(rc.c, okWire); err != nil {
			t.Fatalf("write L0: %v", err)
		}
		_ = rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line0, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L0: %v", err)
		}
		if !strings.HasPrefix(line0, "L0 OK ") {
			t.Fatalf("control 4096-byte LIST () ... RETURN (STATUS (MESSAGES)) failed: %q", strings.TrimRight(line0, "\r\n"))
		}

		// 1 byte larger (4097 bytes): must fail with NO [TOOBIG], NOT BAD [CLIENTBUG] LIST-EXTENDED is not supported.
		pat4097 := strings.Repeat("x", 4097)
		raw := fmt.Sprintf("L1 LIST () \"\" {%d+}\r\n%s RETURN (STATUS (MESSAGES))\r\nL2 NOOP\r\n", len(pat4097), pat4097)
		if _, err := fmt.Fprint(rc.c, raw); err != nil {
			t.Fatalf("write L1: %v", err)
		}
		line1, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L1: %v", err)
		}
		line2, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L2: %v", err)
		}
		if !strings.HasPrefix(line2, "L2 OK ") {
			t.Fatalf("stream out of sync on L2: %q", strings.TrimRight(line2, "\r\n"))
		}
		if !strings.HasPrefix(line1, "L1 NO [TOOBIG] ") {
			t.Fatalf("want L1 NO [TOOBIG], got %q", strings.TrimRight(line1, "\r\n"))
		}
	})

	t.Run("synchronizing 4097-byte ref or pattern literal with standalone option capability", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			caps imap.CapSet
			cmd  string
		}{
			{
				name: "LIST-STATUS oversized reference literal",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
				cmd:  `LIST () {4097}`,
			},
			{
				name: "LIST-STATUS oversized pattern literal",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
				cmd:  `LIST () "" {4097}`,
			},
			{
				name: "LIST-METADATA oversized pattern literal",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMetadata: {}, imap.CapListMetadata: {}},
				cmd:  `LIST () "" {4097}`,
			},
			{
				name: "SPECIAL-USE oversized pattern literal",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}},
				cmd:  `LIST () "" {4097}`,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rc := newSearchRev2ConnWrapped(t, tc.caps, func(sess imapserver.Session) imapserver.Session {
					return &listOptionSession{Session: sess}
				})
				tagged, contReqs := rc.exchange(tc.cmd)
				if !strings.Contains(tagged, " NO [TOOBIG] ") || contReqs != 0 {
					t.Fatalf("%s = %q (cont=%d), want NO [TOOBIG] without continuation", tc.cmd, strings.TrimRight(tagged, "\r\n"), contReqs)
				}
				rc.requireInSync(tc.cmd)
			})
		}
	})

	t.Run("disallowed extended syntax still answers BAD before oversized literal", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			caps imap.CapSet
			cmd  string
		}{
			{
				name: "no standalone option caps at all",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}},
				cmd:  `LIST () {4097}`,
			},
			{
				name: "LIST-STATUS with parenthesized pattern list",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
				cmd:  `LIST "" ({4097}`,
			},
			{
				name: "LIST-STATUS with SUBSCRIBED select option",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
				cmd:  `LIST (SUBSCRIBED) {4097}`,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rc := newSearchRev2ConnWrapped(t, tc.caps, func(sess imapserver.Session) imapserver.Session {
					return &listOptionSession{Session: sess}
				})
				tagged, contReqs := rc.exchange(tc.cmd)
				if !strings.Contains(tagged, " BAD [CLIENTBUG] ") || contReqs != 0 {
					t.Fatalf("%s = %q (cont=%d), want BAD [CLIENTBUG] without continuation", tc.cmd, strings.TrimRight(tagged, "\r\n"), contReqs)
				}
				rc.requireInSync(tc.cmd)
			})
		}
	})
}

func TestUnadvertisedESortBeforeOversizedLiteralAnswersBad(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet
	}{
		{
			name: "server without ESORT",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}},
		},
		{
			name:    "session filtered ESORT out",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}, imap.CapESort: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}},
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
			rc.appendOne("msg1")
			rc.do("SELECT INBOX")

			for _, cmd := range []string{
				`SORT RETURN (MIN) (ARRIVAL) {4097}`,
				`SORT RETURN (MIN) (ARRIVAL) UTF-8 TEXT {4097}`,
				`SORT RETURN () (ARRIVAL) UTF-8 TEXT {4097}`,
				`UID SORT RETURN (COUNT) (ARRIVAL) UTF-8 TEXT {4097}`,
			} {
				tagged, contReqs := rc.exchange(cmd)
				if !strings.Contains(tagged, " BAD [CLIENTBUG] ") || contReqs != 0 {
					t.Errorf("%s = %q (cont=%d), want BAD [CLIENTBUG] without continuation", cmd, strings.TrimRight(tagged, "\r\n"), contReqs)
				}
				rc.requireInSync(cmd)
			}
		})
	}
}
