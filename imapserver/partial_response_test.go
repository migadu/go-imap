package imapserver_test

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
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

type customSearchSession struct {
	imapserver.Session
	retAll imap.NumSet
	retNil bool
}

func (s *customSearchSession) Namespace(ctx context.Context) (*imap.NamespaceData, error) {
	return s.Session.(imapserver.SessionNamespace).Namespace(ctx)
}

func (s *customSearchSession) Move(ctx context.Context, w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	return s.Session.(imapserver.SessionMove).Move(ctx, w, numSet, dest)
}

func (s *customSearchSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if s.retNil {
		return nil, nil
	}
	data, err := s.Session.Search(ctx, kind, criteria, options)
	if data != nil {
		data.All = s.retAll
	}
	return data, err
}

func (s *customSearchSession) MultiSearch(ctx context.Context, source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	if s.retNil {
		return []*imap.SearchData{
			{Mailbox: "INBOX", UIDValidity: 1, All: imap.UIDSet{imap.UIDRange{Start: 1, Stop: 1}}},
			nil,
		}, nil
	}
	return []*imap.SearchData{
		{Mailbox: "INBOX", UIDValidity: 1, All: imap.UIDSet{imap.UIDRange{Start: 1, Stop: 1}}},
		{Mailbox: "Archive", UIDValidity: 2, All: s.retAll},
	}, nil
}

func TestESearchPreWriteValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   imap.CapSet
		enable bool
		retAll imap.NumSet
		retNil bool
		cmd    string
	}{
		{
			name:   "SEARCH RETURN (ALL) with 1:*",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}},
			retAll: imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}},
			cmd:    "SEARCH RETURN (ALL) ALL",
		},
		{
			name:   "SEARCH RETURN (ALL) with SearchRes ($)",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}},
			retAll: imap.SearchRes(),
			cmd:    "SEARCH RETURN (ALL) ALL",
		},
		{
			name:   "plain SEARCH with SearchRes ($)",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}},
			retAll: imap.SearchRes(),
			cmd:    "SEARCH ALL",
		},
		{
			name:   "IMAP4rev2 SEARCH with 1:*",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
			enable: true,
			retAll: imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}},
			cmd:    "SEARCH ALL",
		},
		{
			name:   "MultiSearch with dynamic second mailbox",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.Cap("MULTISEARCH"): {}},
			retAll: imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}},
			cmd:    "ESEARCH IN (personal) ALL",
		},
		{
			name:   "MultiSearch with nil second mailbox",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.Cap("MULTISEARCH"): {}},
			retNil: true,
			cmd:    "ESEARCH IN (personal) ALL",
		},
		{
			name:   "SEARCH with nil SearchData",
			caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}},
			retNil: true,
			cmd:    "SEARCH RETURN (ALL) ALL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrap := func(s imapserver.Session) imapserver.Session {
				return &customSearchSession{
					Session: s,
					retAll:  tc.retAll,
					retNil:  tc.retNil,
				}
			}
			rc := newSearchRev2ConnWrapped(t, tc.caps, wrap)
			if tc.enable {
				rc.do("ENABLE IMAP4rev2")
			}
			rc.appendOne("msg")
			rc.do("SELECT INBOX")

			if _, err := rc.c.Write([]byte("t99 " + tc.cmd + "\r\n")); err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			for {
				line, err := rc.br.ReadString('\n')
				sb.WriteString(line)
				if strings.HasPrefix(line, "t99 ") || err != nil {
					if err != nil || !strings.HasPrefix(line, "t99 NO [SERVERBUG] ") || sb.String() != line {
						t.Errorf("%s: want only tagged NO [SERVERBUG], got err=%v:\n%s", tc.cmd, err, sb.String())
					} else {
						rc.do("NOOP")
					}
					break
				}
			}
		})
	}

	t.Run("imapclient receives NO instead of corrupt ESEARCH ALL", func(t *testing.T) {
		memUser := imapmemserver.NewUser("user", "pass")
		_ = memUser.Create(context.Background(), "INBOX", nil)
		memServer := imapmemserver.New()
		memServer.AddUser(memUser)

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &customSearchSession{
					Session: memServer.NewSession(),
					retAll:  imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}},
				}, nil, nil
			},
			Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}},
			InsecureAuth: true,
		})
		defer srv.Close()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln)

		c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()

		if err := c.Login("user", "pass").Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		_, err = c.Search(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true}).Wait()
		if err != nil && strings.Contains(err.Error(), "dynamic ALL number set") {
			t.Errorf("server emitted illegal dynamic ESEARCH ALL onto the wire: %v", err)
		}
	})
}

type badStatusDataSession struct {
	imapserver.Session
	mutate func(*imap.StatusData) *imap.StatusData
}

func (s *badStatusDataSession) Status(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	data, err := s.Session.Status(ctx, mailbox, options)
	if err != nil {
		return nil, err
	}
	return s.mutate(data), nil
}

func TestStatusPreWriteValidation(t *testing.T) {
	t.Run("negative Size neither panics nor flushes partial STATUS", func(t *testing.T) {
		neg := int64(-5)
		wrap := func(s imapserver.Session) imapserver.Session {
			return &badStatusDataSession{
				Session: s,
				mutate: func(d *imap.StatusData) *imap.StatusData {
					d.Mailbox = strings.Repeat("x", 5000) + "\n"
					d.Size = &neg
					return d
				},
			}
		}
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapStatusSize: {}}, wrap)
		rc.do("ENABLE UTF8=ACCEPT")
		if _, err := rc.c.Write([]byte("t99 STATUS INBOX (MESSAGES SIZE)\r\n")); err != nil {
			t.Fatal(err)
		}
		line, err := rc.br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "t99 NO [SERVERBUG] ") {
			rest, _ := io.ReadAll(rc.br)
			t.Fatalf("STATUS with negative Size got %q (err=%v), want clean t99 NO [SERVERBUG]", line+string(rest), err)
		}
		rc.do("NOOP")
	})

	t.Run("nil StatusData returns NO SERVERBUG without panicking", func(t *testing.T) {
		wrap := func(s imapserver.Session) imapserver.Session {
			return &badStatusDataSession{
				Session: s,
				mutate:  func(*imap.StatusData) *imap.StatusData { return nil },
			}
		}
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}}, wrap)
		if _, err := rc.c.Write([]byte("t99 STATUS INBOX (MESSAGES)\r\n")); err != nil {
			t.Fatal(err)
		}
		line, err := rc.br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "t99 NO [SERVERBUG] ") {
			t.Fatalf("STATUS with nil StatusData got %q (err=%v), want t99 NO [SERVERBUG]", line, err)
		}
		rc.do("NOOP")
	})

	t.Run("unset UIDNext or UIDValidity returns NO SERVERBUG instead of 0", func(t *testing.T) {
		wrap := func(s imapserver.Session) imapserver.Session {
			return &badStatusDataSession{
				Session: s,
				mutate: func(d *imap.StatusData) *imap.StatusData {
					d.UIDNext = 0
					d.UIDValidity = 0
					return d
				},
			}
		}
		rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}}, wrap)
		resp, tagged := tryStopAtGluedTag(rc, "STATUS INBOX (UIDNEXT UIDVALIDITY)")
		if !strings.Contains(tagged, " NO [SERVERBUG] ") || resp != tagged {
			t.Errorf("STATUS with unset UIDNext/UIDValidity: want NO [SERVERBUG], got:\n%s", resp)
		}
		rc.do("NOOP")
	})
}
