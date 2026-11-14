package imapserver_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// metadataSession adds METADATA to the in-memory session and records what
// SETMETADATA stored.
type metadataSession struct {
	imapserver.Session
	set map[string]*[]byte
}

func (s *metadataSession) GetMetadata(ctx context.Context, mailbox string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	return &imap.GetMetadataData{Mailbox: mailbox, Entries: map[string]*[]byte{}}, nil
}

func (s *metadataSession) SetMetadata(ctx context.Context, mailbox string, entries map[string]*[]byte) error {
	s.set = entries
	return nil
}

// metadataMaxSizeSession is a metadataSession that accepts values up to
// maxSize octets.
type metadataMaxSizeSession struct {
	*metadataSession
	maxSize uint32
}

func (s *metadataMaxSizeSession) MetadataMaxSize() uint32 { return s.maxSize }

func dialMetadataConn(t *testing.T, wrap func(imapserver.Session) imapserver.Session) *searchRev2Conn {
	t.Helper()
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMetadata: {}, imap.CapLiteralPlus: {}}
	rc := dialSearchRev2Conn(t, caps, wrap)
	rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)
	return rc
}

// A SETMETADATA value sent as a literal is limited by what the session says it
// accepts, not by the 4096 octets other buffered literals are held to: with a
// 64 KiB limit a 5000-octet value was refused with NO [TOOBIG] "Literals are
// limited to 4096 bytes for this command" before the backend saw it.
func TestSetMetadataLiteralUpToSessionMaxSize(t *testing.T) {
	meta := &metadataSession{}
	rc := dialMetadataConn(t, func(sess imapserver.Session) imapserver.Session {
		meta.Session = sess
		return &metadataMaxSizeSession{metadataSession: meta, maxSize: 64 * 1024}
	})

	value := strings.Repeat("x", 5000)
	tagged, contReqs := rc.exchange(
		`SETMETADATA "" (/private/comment {`+strconv.Itoa(len(value))+`}`,
		value+")",
	)
	if !strings.Contains(tagged, " OK ") || contReqs != 1 {
		t.Fatalf("SETMETADATA with a 5000-octet literal and a 64 KiB limit = %q after %d continuation requests, want OK after 1", strings.TrimRight(tagged, "\r\n"), contReqs)
	}
	if got := meta.set["/private/comment"]; got == nil || len(*got) != len(value) {
		t.Fatalf("stored value = %v, want %d octets", got, len(value))
	}
}

// RFC 5464 §4.3: "If the server is unable to set an annotation because the
// size of its value is too large, the server MUST return a tagged NO response
// with a "[METADATA MAXSIZE NNN]" response code when NNN is the maximum octet
// count that it is willing to accept." A literal over the limit is refused by
// its announced size, before the client sends it.
func TestSetMetadataLiteralOverMaxSizeRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxSize uint32 // 0: the session does not implement SessionMetadataMaxSize
		size    int
		want    string
	}{
		{"session limit", 1024, 2000, "NO [METADATA MAXSIZE 1024]"},
		{"default limit", 0, 5000, "NO [METADATA MAXSIZE 4096]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := &metadataSession{}
			rc := dialMetadataConn(t, func(sess imapserver.Session) imapserver.Session {
				meta.Session = sess
				if tc.maxSize == 0 {
					return meta
				}
				return &metadataMaxSizeSession{metadataSession: meta, maxSize: tc.maxSize}
			})

			tagged, contReqs := rc.exchange(`SETMETADATA "" (/private/comment {` + strconv.Itoa(tc.size) + `}`)
			if contReqs != 0 || !strings.Contains(tagged, " "+tc.want+" ") {
				t.Fatalf("SETMETADATA with a %d-octet literal = %q after %d continuation requests, want %q at once", tc.size, strings.TrimRight(tagged, "\r\n"), contReqs, tc.want)
			}
			if meta.set != nil {
				t.Fatalf("SetMetadata was called with %v", meta.set)
			}
			rc.do("NOOP") // the connection is still in sync
		})
	}
}

// A non-synchronizing literal over the limit is refused the same way, and its
// octets, already on the wire, are skipped rather than read as a command.
func TestSetMetadataNonSyncLiteralOverMaxSizeRefused(t *testing.T) {
	meta := &metadataSession{}
	rc := dialMetadataConn(t, func(sess imapserver.Session) imapserver.Session {
		meta.Session = sess
		return &metadataMaxSizeSession{metadataSession: meta, maxSize: 1024}
	})

	value := strings.Repeat("x", 2000)
	_, tagged := rc.try(`SETMETADATA "" (/private/comment {` + strconv.Itoa(len(value)) + "+}\r\n" + value + ")")
	if !strings.Contains(tagged, " NO [METADATA MAXSIZE 1024] ") {
		t.Fatalf("SETMETADATA with a 2000-octet non-synchronizing literal = %q, want NO [METADATA MAXSIZE 1024]", strings.TrimRight(tagged, "\r\n"))
	}
	if meta.set != nil {
		t.Fatalf("SetMetadata was called with %v", meta.set)
	}
	rc.do("NOOP")
}
