package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestListRepeatedStatusReturnOption(t *testing.T) {
	t.Run("repeated STATUS hides HIGHESTMODSEQ without CONDSTORE (regression)", func(t *testing.T) {
		var probe *listOptionSession
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapListExtended: {},
			imap.CapListStatus:   {},
		}, func(sess imapserver.Session) imapserver.Session {
			probe = &listOptionSession{Session: sess}
			return probe
		})

		resp, tagged := rc.try(`LIST "" "INBOX" RETURN (STATUS (HIGHESTMODSEQ) STATUS (MESSAGES))`)
		if !strings.Contains(tagged, " BAD ") {
			t.Fatalf("expected BAD when HIGHESTMODSEQ requested without CONDSTORE, got %q", resp)
		}
		if probe.listCalls.Load() != 0 {
			t.Fatalf("rejected LIST command reached backend (%d calls)", probe.listCalls.Load())
		}
	})

	t.Run("repeated STATUS loses CONDSTORE-enabling side effect and HIGHESTMODSEQ item (regression)", func(t *testing.T) {
		rc := newSearchRev2Conn(t, imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapListExtended: {},
			imap.CapListStatus:   {},
			imap.CapCondStore:    {},
		})
		rc.appendOne("msg1")

		resp, tagged := rc.try(`LIST "" "INBOX" RETURN (STATUS (HIGHESTMODSEQ) STATUS (MESSAGES))`)
		if !strings.Contains(tagged, " OK ") {
			t.Fatalf("expected OK, got %q", resp)
		}
		if !strings.Contains(resp, "HIGHESTMODSEQ") || !strings.Contains(resp, "MESSAGES 1") {
			t.Errorf("expected both HIGHESTMODSEQ and MESSAGES in STATUS response, got %q", resp)
		}
		rc.do("SELECT INBOX")
		storeResp := rc.do(`STORE 1 +FLAGS (\Seen)`)
		if !strings.Contains(storeResp, "MODSEQ (") {
			t.Errorf("expected CONDSTORE to be enabled by LIST RETURN (STATUS (HIGHESTMODSEQ) ...), STORE response was %q", storeResp)
		}
	})

	t.Run("repeated STATUS hides RECENT on IMAP4rev2-only server", func(t *testing.T) {
		var probe *listOptionSession
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{
			imap.CapIMAP4rev2: {},
		}, func(sess imapserver.Session) imapserver.Session {
			probe = &listOptionSession{Session: sess}
			return &rev2ListOptionSession{
				listOptionSession: probe,
				rev2:              sess.(imapserver.SessionIMAP4rev2),
			}
		})

		resp, tagged := rc.try(`LIST "" "INBOX" RETURN (STATUS (RECENT) STATUS (MESSAGES))`)
		if !strings.Contains(tagged, " BAD ") {
			t.Fatalf("expected BAD for RECENT on IMAP4rev2-only server, got %q", resp)
		}
		if probe.listCalls.Load() != 0 {
			t.Fatalf("rejected LIST command reached backend")
		}
	})

	t.Run("repeated STATUS hides RECENT after ENABLE IMAP4rev2", func(t *testing.T) {
		var probe *listOptionSession
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		}, func(sess imapserver.Session) imapserver.Session {
			probe = &listOptionSession{Session: sess}
			return &rev2ListOptionSession{
				listOptionSession: probe,
				rev2:              sess.(imapserver.SessionIMAP4rev2),
			}
		})
		rc.do("ENABLE IMAP4rev2")

		resp, tagged := rc.try(`LIST "" "INBOX" RETURN (STATUS (RECENT) STATUS (MESSAGES))`)
		if !strings.Contains(tagged, " BAD ") {
			t.Fatalf("expected BAD for RECENT after ENABLE IMAP4rev2, got %q", resp)
		}
		if probe.listCalls.Load() != 0 {
			t.Fatalf("rejected LIST command reached backend")
		}
	})
}

func TestListAndGetMetadataBadPrecedenceBeforeOversizedLiteral(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{
			name: "GETMETADATA invalid entry before oversized entry literal",
			cmd:  `GETMETADATA INBOX (/private/ {4097}`,
		},
		{
			name: "GETMETADATA server invalid entry before oversized entry literal",
			cmd:  `GETMETADATA "" (/bad//name {4097}`,
		},
		{
			name: "LIST RECURSIVEMATCH without SUBSCRIBED before oversized reference literal",
			cmd:  `LIST (RECURSIVEMATCH) {4097}`,
		},
		{
			name: "LIST RECURSIVEMATCH without SUBSCRIBED before oversized pattern literal",
			cmd:  `LIST (RECURSIVEMATCH) "" {4097}`,
		},
		{
			name: "LIST RECURSIVEMATCH without SUBSCRIBED before oversized return metadata literal",
			cmd:  `LIST (RECURSIVEMATCH) "" INBOX RETURN (METADATA ({4097}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var session *listOptionSession
			rc := newSearchRev2ConnWrapped(t, imap.CapSet{
				imap.CapIMAP4rev1:    {},
				imap.CapListExtended: {},
				imap.CapListStatus:   {},
				imap.CapMetadata:     {},
				imap.CapListMetadata: {},
			}, func(sess imapserver.Session) imapserver.Session {
				session = &listOptionSession{Session: sess}
				return session
			})

			tagged, contReqs := rc.exchange(tc.cmd)
			if !strings.Contains(tagged, " BAD ") || contReqs != 0 {
				t.Errorf("%s = %q after %d continuation requests, want BAD without continuation", tc.cmd, strings.TrimRight(tagged, "\r\n"), contReqs)
			}
			if session.listCalls.Load() != 0 || session.metadataCalls.Load() != 0 {
				t.Error("rejected command reached the backend")
			}
			rc.requireInSync(tc.cmd)
		})
	}

	t.Run("GETMETADATA invalid entry before oversized non-sync LITERAL+ entry literal", func(t *testing.T) {
		var session *listOptionSession
		caps := imap.CapSet{
			imap.CapIMAP4rev1:   {},
			imap.CapLiteralPlus: {},
			imap.CapMetadata:    {},
		}
		rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
			session = &listOptionSession{Session: sess}
			return session
		})
		rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

		payload := "/private/" + strings.Repeat("a", 4089) // 4098 bytes (> 4096 buffered limit)
		raw := fmt.Sprintf("M1 GETMETADATA \"\" (/bad//name {%d+}\r\n%s)\r\nM2 NOOP\r\n", len(payload), payload)
		if _, err := fmt.Fprint(rc.c, raw); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line1, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read M1: %v", err)
		}
		if !strings.HasPrefix(line1, "M1 BAD ") {
			t.Fatalf("want M1 BAD, got %q", strings.TrimRight(line1, "\r\n"))
		}
		line2, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read M2: %v", err)
		}
		if !strings.HasPrefix(line2, "M2 OK ") {
			t.Fatalf("want M2 OK, got %q", strings.TrimRight(line2, "\r\n"))
		}
		if session.metadataCalls.Load() != 0 {
			t.Error("rejected GETMETADATA reached backend")
		}
	})

	t.Run("LIST STATUS HIGHESTMODSEQ without CONDSTORE before oversized literal", func(t *testing.T) {
		var session *listOptionSession
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapListExtended: {},
			imap.CapListStatus:   {},
			imap.CapMetadata:     {},
			imap.CapListMetadata: {},
		}, func(sess imapserver.Session) imapserver.Session {
			session = &listOptionSession{Session: sess}
			return session
		})

		tagged, contReqs := rc.exchange(`LIST "" INBOX RETURN (STATUS (HIGHESTMODSEQ) METADATA ({4097}`)
		if !strings.Contains(tagged, " BAD ") || contReqs != 0 {
			t.Fatalf("got %q (cont=%d), want BAD without continuation", strings.TrimRight(tagged, "\r\n"), contReqs)
		}
		if session.listCalls.Load() != 0 {
			t.Fatal("rejected LIST reached backend")
		}
		rc.requireInSync("LIST STATUS HIGHESTMODSEQ")
	})

	t.Run("LIST STATUS RECENT on IMAP4rev2 before oversized literal", func(t *testing.T) {
		var probe *listOptionSession
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapIMAP4rev2:    {},
			imap.CapMetadata:     {},
			imap.CapListMetadata: {},
		}, func(sess imapserver.Session) imapserver.Session {
			probe = &listOptionSession{Session: sess}
			return &rev2ListOptionSession{
				listOptionSession: probe,
				rev2:              sess.(imapserver.SessionIMAP4rev2),
			}
		})
		rc.do("ENABLE IMAP4rev2")

		tagged, contReqs := rc.exchange(`LIST "" INBOX RETURN (STATUS (RECENT) METADATA ({4097}`)
		if !strings.Contains(tagged, " BAD ") || contReqs != 0 {
			t.Fatalf("got %q (cont=%d), want BAD without continuation", strings.TrimRight(tagged, "\r\n"), contReqs)
		}
		if probe.listCalls.Load() != 0 {
			t.Fatal("rejected LIST reached backend")
		}
		rc.requireInSync("LIST STATUS RECENT")
	})
}

func TestListRepeatedMetadataReturnOptionMergesEntries(t *testing.T) {
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{
		imap.CapIMAP4rev1:    {},
		imap.CapMetadata:     {},
		imap.CapListExtended: {},
		imap.CapListMetadata: {},
	}, nil)

	respSet, taggedSet := rc.try(`SETMETADATA "INBOX" ("/private/comment" "priv-val" "/shared/comment" "shared-val")`)
	if !strings.Contains(taggedSet, " OK ") {
		t.Fatalf("SETMETADATA failed: %q", respSet)
	}

	t.Run("two explicit METADATA return options merge entries", func(t *testing.T) {
		resp, tagged := rc.try(`LIST "" "INBOX" RETURN (METADATA (/private/comment) METADATA (/shared/comment))`)
		if !strings.Contains(tagged, " OK ") {
			t.Fatalf("expected OK, got %q", resp)
		}
		if !strings.Contains(resp, "/private/comment") || !strings.Contains(resp, "/shared/comment") {
			t.Fatalf("expected both /private/comment and /shared/comment in METADATA response (RFC 5258 §3.3), got:\n%s", resp)
		}
	})

	t.Run("bare METADATA followed by explicit METADATA retains all entries", func(t *testing.T) {
		resp, tagged := rc.try(`LIST "" "INBOX" RETURN (METADATA METADATA (/private/comment))`)
		if !strings.Contains(tagged, " OK ") {
			t.Fatalf("expected OK, got %q", resp)
		}
		if !strings.Contains(resp, "/private/comment") || !strings.Contains(resp, "/shared/comment") {
			t.Fatalf("expected bare METADATA (all entries) to retain /shared/comment as well as /private/comment, got:\n%s", resp)
		}
	})
}

func TestGetMetadataEmptyOptionsWithNonSyncLiteralStaysInSync(t *testing.T) {
	meta := &metadataSession{}
	caps := imap.CapSet{
		imap.CapIMAP4rev1:   {},
		imap.CapLiteralPlus: {},
		imap.CapMetadata:    {},
	}
	rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
		meta.Session = sess
		return meta
	})
	rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

	raw := "M1 GETMETADATA () INBOX ({16+}\r\n/private/comment)\r\nM2 NOOP\r\n"
	if _, err := fmt.Fprint(rc.c, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line1, err := rc.br.ReadString('\n')
	if err != nil {
		t.Fatalf("read M1: %v", err)
	}
	if !strings.HasPrefix(line1, "M1 BAD ") {
		t.Fatalf("want M1 BAD, got %q", strings.TrimRight(line1, "\r\n"))
	}
	line2, err := rc.br.ReadString('\n')
	if err != nil {
		t.Fatalf("read M2: %v", err)
	}
	if !strings.HasPrefix(line2, "M2 OK ") {
		t.Fatalf("want M2 OK, got %q", strings.TrimRight(line2, "\r\n"))
	}
}
