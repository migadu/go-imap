package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestListBareMetadataBeforeOtherReturnOptions(t *testing.T) {
	t.Run("bare METADATA followed by another valid option", func(t *testing.T) {
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapMetadata:     {},
			imap.CapListExtended: {},
			imap.CapListMetadata: {},
		}, func(sess imapserver.Session) imapserver.Session {
			return &listOptionSession{Session: sess}
		})

		for _, cmd := range []string{
			`LIST "" "INBOX" RETURN (SUBSCRIBED METADATA)`,
			`LIST "" "INBOX" RETURN (METADATA SUBSCRIBED)`,
			`LIST "" "INBOX" RETURN (METADATA METADATA (/private/comment))`,
		} {
			resp, tagged := rc.try(cmd)
			if !strings.Contains(tagged, " OK ") {
				t.Errorf("%s -> %q, want OK", cmd, resp)
			}
		}
	})

	t.Run("bare METADATA followed by METADATA with non-sync literal stays in sync", func(t *testing.T) {
		caps := imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapMetadata:     {},
			imap.CapListExtended: {},
			imap.CapListMetadata: {},
		}
		rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
			return &listOptionSession{Session: sess}
		})
		rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

		raw := "M1 LIST \"\" \"INBOX\" RETURN (METADATA METADATA ({9+}\r\n/private/))\r\nM2 NOOP\r\n"
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
	})
}

func TestListReturnStatusMidParseErrorsAndStatePrecedence(t *testing.T) {
	t.Run("STATUS HIGHESTMODSEQ without CONDSTORE followed by non-sync literal", func(t *testing.T) {
		caps := imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapMetadata:     {},
			imap.CapListExtended: {},
			imap.CapListMetadata: {},
		}
		rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
			return &listOptionSession{Session: sess}
		})
		rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

		raw := "L1 LIST \"\" \"*\" RETURN (STATUS (HIGHESTMODSEQ) METADATA ({16+}\r\n/private/comment))\r\nL2 NOOP\r\n"
		if _, err := fmt.Fprint(rc.c, raw); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line1, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L1: %v", err)
		}
		if !strings.HasPrefix(line1, "L1 BAD ") {
			t.Fatalf("want L1 BAD, got %q", strings.TrimRight(line1, "\r\n"))
		}
		line2, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L2: %v", err)
		}
		if !strings.HasPrefix(line2, "L2 OK ") {
			t.Fatalf("want L2 OK, got %q", strings.TrimRight(line2, "\r\n"))
		}
	})

	t.Run("unauthenticated LIST RETURN (STATUS (HIGHESTMODSEQ)) checks state before capability", func(t *testing.T) {
		caps := imap.CapSet{imap.CapIMAP4rev1: {}}
		rc := dialSearchRev2Conn(t, caps, nil)

		_, taggedSpecialUse := rc.try(`LIST (SPECIAL-USE) "" "*"`)
		_, taggedHighestModSeq := rc.try(`LIST "" "*" RETURN (STATUS (HIGHESTMODSEQ))`)
		if strings.Contains(taggedHighestModSeq, "HIGHESTMODSEQ requires CONDSTORE") {
			t.Errorf("unauthenticated LIST leaked capability error before state check: got %q (compare SPECIAL-USE: %q)",
				strings.TrimRight(taggedHighestModSeq, "\r\n"),
				strings.TrimRight(taggedSpecialUse, "\r\n"))
		}
	})
}

func TestMetadataOptionSyncAndSetMetadataBadPrecedence(t *testing.T) {
	t.Run("GETMETADATA invalid option followed by non-sync literal stays in sync", func(t *testing.T) {
		meta := &metadataSession{}
		caps := imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapMetadata:  {},
		}
		rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
			meta.Session = sess
			return meta
		})
		rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

		raw := "M1 GETMETADATA (DEPTH 2) INBOX ({16+}\r\n/private/comment)\r\nM2 NOOP\r\n"
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
	})

	t.Run("SETMETADATA invalid entry name with oversized value answers BAD not NO MAXSIZE", func(t *testing.T) {
		meta := &metadataSession{}
		rc := dialMetadataConn(t, func(sess imapserver.Session) imapserver.Session {
			meta.Session = sess
			return &metadataMaxSizeSession{metadataSession: meta, maxSize: 100}
		})

		tagged, contReqs := rc.exchange(`SETMETADATA "" ("/private/" {200}`)
		if contReqs != 0 || !strings.Contains(tagged, " BAD ") {
			t.Fatalf("SETMETADATA invalid entry '/private/' with {200} (maxSize=100) = %q, want BAD (RFC 5464 §3.2)", strings.TrimRight(tagged, "\r\n"))
		}
	})
}
