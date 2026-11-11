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

// RFC 9051 Appendix B: IMAP4rev2 has BINARY's FETCH items "but not extensions
// to the APPEND command". The literal8 APPEND of RFC 3516 §4.4 is BINARY's
// alone, so a server that does not offer BINARY answers it BAD, as a command
// it does not implement (RFC 9051 §7.1.3), and does not ask for the data. The
// octets of a non-synchronizing literal are read through. Issue #169.
func TestAppendBinaryNeedsBinaryCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps imap.CapSet
	}{
		{"IMAP4rev1", imap.CapSet{imap.CapIMAP4rev1: {}}},
		{"IMAP4rev2", imap.CapSet{imap.CapIMAP4rev2: {}}},
		{"IMAP4rev1 and IMAP4rev2", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, tc.caps)

			if _, tagged := rc.try("APPEND INBOX ~{5+}\r\nab\x00cd"); !strings.Contains(tagged, " BAD ") {
				t.Errorf("APPEND ~{5+} → %q, want BAD", strings.TrimRight(tagged, "\r\n"))
			}
			rc.do("NOOP")

			// A synchronizing literal8 gets no continuation request.
			if _, err := rc.c.Write([]byte("s1 APPEND INBOX ~{5}\r\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
			line, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading the answer to APPEND ~{5}: %v", err)
			}
			if !strings.HasPrefix(line, "s1 BAD ") {
				t.Fatalf("APPEND ~{5} → %q, want s1 BAD", strings.TrimRight(line, "\r\n"))
			}
			rc.do("NOOP")

			if resp := rc.do("STATUS INBOX (MESSAGES)"); !strings.Contains(resp, "(MESSAGES 0)") {
				t.Errorf("STATUS after the refused APPENDs → %q, want no message stored", resp)
			}
		})
	}
}
