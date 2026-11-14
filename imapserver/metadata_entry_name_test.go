package imapserver_test

import (
	"strings"
	"testing"

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
