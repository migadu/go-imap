package imapserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// getMetadataRecorder is a metadataSession that records the GETMETADATA calls
// that reach it.
type getMetadataRecorder struct {
	metadataSession
	calls   int
	options *imap.GetMetadataOptions
}

func (s *getMetadataRecorder) GetMetadata(ctx context.Context, mailbox string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	s.calls++
	s.options = options
	return s.metadataSession.GetMetadata(ctx, mailbox, entries, options)
}

// RFC 5464 §5:
//
//	getmetadata       = "GETMETADATA" [SP getmetadata-options]
//	                                  SP mailbox SP entries
//	getmetadata-options = "(" getmetadata-option
//	                          *(SP getmetadata-option) ")"
//
// An options list holds at least one option and is always followed by SP.
// §4.2: "BAD - command unknown or arguments invalid". An empty list "()" was
// taken as no options, so the SP after it was not required either:
// "GETMETADATA ()INBOX ..." was answered OK, and "GETMETADATA () INBOX ..." BAD
// only for the SP, with a message about an ASTRING-CHAR.
func TestGetMetadataOptionsListSyntax(t *testing.T) {
	rec := &getMetadataRecorder{}
	rc := dialMetadataConn(t, func(sess imapserver.Session) imapserver.Session {
		rec.Session = sess
		return rec
	})

	// Control: without a list the session gets no options, with one it gets
	// what the list holds.
	rc.do(`GETMETADATA INBOX (/private/comment)`)
	if rec.calls != 1 || rec.options != nil {
		t.Fatalf("GETMETADATA without options: %d calls, options %+v, want 1 call with nil options", rec.calls, rec.options)
	}
	rc.do(`GETMETADATA (DEPTH 1) INBOX (/private/comment)`)
	if rec.calls != 2 || rec.options == nil || rec.options.Depth != imap.GetMetadataDepthOne {
		t.Fatalf("GETMETADATA (DEPTH 1): %d calls, options %+v, want 2 calls with DEPTH 1", rec.calls, rec.options)
	}
	rec.calls = 0

	for _, tc := range []struct {
		cmd       string
		emptyList bool // the BAD text must name the options
	}{
		{`GETMETADATA ()INBOX (/private/comment)`, true},
		{`GETMETADATA ()"" (/private/comment)`, true},
		{`GETMETADATA () INBOX (/private/comment)`, true},
		{`GETMETADATA () "" /private/comment`, true},
		{`GETMETADATA (DEPTH 1)INBOX (/private/comment)`, false},
		{`GETMETADATA (MAXSIZE 1024)"" (/private/comment)`, false},
	} {
		_, tagged := rc.try(tc.cmd)
		tagged = strings.TrimRight(tagged, "\r\n")
		if !strings.Contains(tagged, " BAD ") {
			t.Errorf("%s → %s, want BAD", tc.cmd, tagged)
		} else if tc.emptyList && !strings.Contains(strings.ToLower(tagged), "option") {
			t.Errorf("%s → %s, want a BAD that names the empty options list", tc.cmd, tagged)
		}
		rc.requireInSync(tc.cmd)
	}
	if rec.calls != 0 {
		t.Errorf("%d malformed GETMETADATA commands reached the session", rec.calls)
	}
}
