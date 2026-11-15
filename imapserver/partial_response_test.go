package imapserver_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// unsetStatusSession answers STATUS without the DELETED item, as a backend
// that does not compute it does.
type unsetStatusSession struct {
	imapserver.Session
}

func (s *unsetStatusSession) Status(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	data, err := s.Session.Status(ctx, mailbox, options)
	if data != nil {
		data.NumDeleted = nil
	}
	return data, err
}

// List answers LIST RETURN (STATUS) for INBOX with the same STATUS data.
func (s *unsetStatusSession) List(ctx context.Context, w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if options.ReturnStatus == nil {
		return s.Session.List(ctx, w, ref, patterns, options)
	}
	data, err := s.Status(ctx, "INBOX", options.ReturnStatus)
	if err != nil {
		return err
	}
	return w.WriteList(&imap.ListData{Delim: '/', Mailbox: "INBOX", Status: data})
}

// dynamicSearchSession answers SEARCH with a set that cannot be enumerated.
type dynamicSearchSession struct {
	imapserver.Session
}

func (s *dynamicSearchSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	data, err := s.Session.Search(ctx, kind, criteria, options)
	if data != nil {
		data.All = imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}} // 1:*
	}
	return data, err
}

// A response the server cannot complete must fail before any of it is written:
// the tagged NO otherwise follows the unterminated untagged line on the same
// line, and the client sees neither response. STATUS checked the data items
// while it encoded them, so a backend that left a requested one unset got
// "* STATUS INBOX (MESSAGES 1t2 NO [SERVERBUG] Internal server error", and
// SEARCH enumerated its result after writing "* SEARCH".
func TestFailedResponseWritesNoPartialLine(t *testing.T) {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}}
	for _, tc := range []struct {
		name     string
		wrap     func(imapserver.Session) imapserver.Session
		selected bool
		cmd      string
		untagged string // the only untagged response wanted, if any
	}{
		{
			name: "STATUS with an item left unset",
			wrap: func(s imapserver.Session) imapserver.Session { return &unsetStatusSession{s} },
			cmd:  "STATUS INBOX (MESSAGES DELETED)",
		},
		{
			name: "STATUS with only an item left unset",
			wrap: func(s imapserver.Session) imapserver.Session { return &unsetStatusSession{s} },
			cmd:  "STATUS INBOX (DELETED)",
		},
		{
			name:     "LIST RETURN (STATUS) with an item left unset",
			wrap:     func(s imapserver.Session) imapserver.Session { return &unsetStatusSession{s} },
			cmd:      `LIST "" "INBOX" RETURN (STATUS (MESSAGES DELETED))`,
			untagged: "* LIST (",
		},
		{
			name:     "SEARCH with a result that cannot be enumerated",
			wrap:     func(s imapserver.Session) imapserver.Session { return &dynamicSearchSession{s} },
			selected: true,
			cmd:      "SEARCH ALL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2ConnWrapped(t, caps, tc.wrap)
			rc.appendOne("partial response")
			if tc.selected {
				rc.do("SELECT INBOX")
			}

			resp, tagged := tryStopAtGluedTag(rc, tc.cmd)
			if !strings.HasPrefix(tagged, "t") || !strings.Contains(tagged, " NO [SERVERBUG] ") {
				t.Fatalf("%s: want a tagged NO [SERVERBUG] on its own line, got:\n%s", tc.cmd, resp)
			}
			untagged := strings.TrimSuffix(resp, tagged)
			if tc.untagged == "" && untagged != "" || tc.untagged != "" && (strings.Count(untagged, "\r\n") != 1 || !strings.HasPrefix(untagged, tc.untagged)) {
				t.Errorf("%s: want untagged %q only, got:\n%s", tc.cmd, tc.untagged, resp)
			}
			// The connection is still in step.
			rc.do("NOOP")
		})
	}
}

// tryStopAtGluedTag is searchRev2Conn.try, except that it also stops at a
// tagged response glued to the end of an unterminated untagged line instead of
// waiting for the connection deadline.
func tryStopAtGluedTag(rc *searchRev2Conn, cmd string) (resp, tagged string) {
	rc.t.Helper()
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	if _, err := rc.c.Write([]byte(tag + " " + cmd + "\r\n")); err != nil {
		rc.t.Fatalf("write %q: %v", cmd, err)
	}
	var sb strings.Builder
	for {
		line, err := rc.br.ReadString('\n')
		sb.WriteString(line)
		if strings.HasPrefix(line, tag+" ") || strings.Contains(line, tag+" NO ") {
			return sb.String(), line
		}
		if err != nil {
			rc.t.Fatalf("reading response to %q: %v (got %q)", cmd, err, sb.String())
		}
	}
}
