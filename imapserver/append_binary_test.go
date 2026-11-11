package imapserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type appendOptionsSession struct {
	imapserver.Session
	got chan imap.AppendOptions
}

func (s *appendOptionsSession) Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	s.got <- *options
	return s.Session.Append(ctx, mailbox, r, options)
}

// RFC 3516 §4.4: "If the destination mailbox does not support the storage of
// binary content, the server MUST fail the request and issue a "NO" response
// that contains the "UNKNOWN-CTE" extended response code." Only the backend
// knows that, so it has to be told the message came as a literal8.
func TestAppendBinaryReachesSession(t *testing.T) {
	got := make(chan imap.AppendOptions, 1)
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}}, func(sess imapserver.Session) imapserver.Session {
		return &appendOptionsSession{Session: sess, got: got}
	})

	for _, tc := range []struct {
		literal string
		binary  bool
	}{
		{"~{5+}\r\nab\x00cd", true},
		{"{5+}\r\nabxcd", false},
	} {
		if _, tagged := rc.try("APPEND INBOX " + tc.literal); !strings.Contains(tagged, " OK ") {
			t.Fatalf("APPEND %q → %q", tc.literal, tagged)
		}
		if options := <-got; options.Binary != tc.binary {
			t.Errorf("APPEND %q: options.Binary = %v, want %v", tc.literal, options.Binary, tc.binary)
		}
	}
}
