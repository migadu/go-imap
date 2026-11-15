package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// RFC 5464 §3.2: "An entry name MUST NOT contain two consecutive "/"
// characters and MUST NOT end with a "/" character", and "Invalid entry names
// result in a BAD response in any IMAP command in which they are used". The
// scope roots /private and /shared, with or without the trailing slash, are
// not entry names: every command naming one is BAD before the session sees it.
func TestMetadataScopeRootEntryIsBAD(t *testing.T) {
	meta := &metadataSession{}
	caps := imap.CapSet{
		imap.CapIMAP4rev1: {}, imap.CapMetadata: {},
		imap.CapListExtended: {}, imap.CapListMetadata: {},
	}
	rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
		meta.Session = sess
		return meta
	})
	rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

	commands := func(name string) []string {
		return []string{
			`SETMETADATA "" (` + name + ` "x")`,
			`SETMETADATA INBOX (` + name + ` "x")`,
			`GETMETADATA "" (` + name + `)`,
			`GETMETADATA (DEPTH infinity) INBOX (` + name + `)`,
			`LIST "" "*" RETURN (METADATA (` + name + `))`,
		}
	}

	// Control: a valid entry name is accepted by every command.
	for _, cmd := range commands("/private/comment") {
		rc.do(cmd)
	}
	meta.set = nil

	for _, name := range []string{"/private/", "/shared/", "/PRIVATE/", "/private", "/shared"} {
		for _, cmd := range commands(name) {
			if _, tagged := rc.try(cmd); !strings.Contains(tagged, " BAD ") {
				t.Errorf("%s → %s, want BAD", cmd, strings.TrimRight(tagged, "\r\n"))
			}
		}
	}
	if meta.set != nil {
		t.Errorf("SETMETADATA reached the session with %v", meta.set)
	}
}

// RFC 5464 §5 defines both GETMETADATA's parenthesized entry list and
// SETMETADATA's entry-values list with a mandatory first element:
//
//	entries      = entry / "(" entry *(SP entry) ")"
//	entry-values = "(" entry-value *(SP entry-value) ")"
//
// Empty "()" must be answered BAD [CLIENTBUG] without reaching the session.
func TestMetadataEmptyRequiredListsAreBAD(t *testing.T) {
	rec := &getMetadataRecorder{}
	rc := dialSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1:      {},
		imap.CapMetadata:       {},
		imap.CapMetadataServer: {},
	}, func(sess imapserver.Session) imapserver.Session {
		rec.Session = sess
		return rec
	})
	rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

	for _, cmd := range []string{
		`GETMETADATA INBOX ()`,
		`GETMETADATA "" ()`,
		`GETMETADATA (MAXSIZE 1024) INBOX ()`,
		`SETMETADATA INBOX ()`,
		`SETMETADATA "" ()`,
	} {
		resp, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
			t.Errorf("%s → %s, want BAD [CLIENTBUG]", cmd, strings.TrimRight(tagged, "\r\n"))
		} else if resp != tagged {
			t.Errorf("%s: want no untagged response, got:\n%s", cmd, resp)
		}
		rc.requireInSync(cmd)
	}
	if rec.calls != 0 || rec.set != nil {
		t.Errorf("malformed METADATA commands reached session: getCalls=%d set=%v", rec.calls, rec.set)
	}
}

// Rejecting an invalid entry name mid-list before consuming a subsequent
// RFC 7888 LITERAL- non-synchronizing literal on the same command causes
// DiscardLine to see the unconsumed "{n+}\r\n" header and terminate the
// connection with "* BYE Unable to resynchronize command stream" instead of
// answering BAD and keeping the stream synchronized.
func TestMetadataInvalidEntryWithNonSyncLiteralStaysInSync(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{
			name: "SETMETADATA scope root with non-sync literal value",
			cmd:  "M1 SETMETADATA \"\" (\"/private/\" {5+}\r\nhello)\r\nM2 NOOP\r\n",
		},
		{
			name: "SETMETADATA bare scope with non-sync literal value",
			cmd:  "M1 SETMETADATA \"\" (\"/private\" {5+}\r\nhello)\r\nM2 NOOP\r\n",
		},
		{
			name: "GETMETADATA scope root followed by non-sync literal entry",
			cmd:  "M1 GETMETADATA \"\" (\"/private/\" {16+}\r\n/private/comment)\r\nM2 NOOP\r\n",
		},
		{
			name: "LIST RETURN METADATA scope root followed by non-sync literal entry",
			cmd:  "M1 LIST \"\" \"*\" RETURN (METADATA (\"/private/\" {16+}\r\n/private/comment))\r\nM2 NOOP\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := &metadataSession{}
			caps := imap.CapSet{
				imap.CapIMAP4rev1:    {},
				imap.CapMetadata:     {},
				imap.CapListExtended: {},
				imap.CapListMetadata: {},
			}
			rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
				meta.Session = sess
				return meta
			})
			rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

			if _, err := fmt.Fprint(rc.c, tc.cmd); err != nil {
				t.Fatalf("write failed: %v", err)
			}

			if err := rc.c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("SetReadDeadline: %v", err)
			}
			line1, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading M1 response: %v", err)
			}
			if !strings.HasPrefix(line1, "M1 BAD ") {
				t.Fatalf("want M1 BAD, got %q", strings.TrimRight(line1, "\r\n"))
			}

			line2, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading M2 response: %v", err)
			}
			if !strings.HasPrefix(line2, "M2 OK ") {
				t.Fatalf("want M2 OK, got %q", strings.TrimRight(line2, "\r\n"))
			}
			if meta.set != nil {
				t.Errorf("SETMETADATA reached session with %v", meta.set)
			}
		})
	}
}
