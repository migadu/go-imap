package imapserver_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type notifyByeLifecycleSession struct {
	imapserver.Session
	dead                      atomic.Bool
	allowUnselectWhenDead     bool
	pollStarted               chan struct{}
	postByePollCalls          atomic.Int32
	postByeSawCondStore       atomic.Bool
	failNotifyPollWhenDead    bool
	emitModSeqInPostByeWindow bool
}

var (
	_ imapserver.SessionNotify         = (*notifyByeLifecycleSession)(nil)
	_ imapserver.SessionUnauthenticate = (*notifyByeLifecycleSession)(nil)
)

func (s *notifyByeLifecycleSession) byeErr() error {
	return &imap.Error{
		Type: imap.StatusResponseTypeBye,
		Text: "Session terminated by backend",
	}
}

func (s *notifyByeLifecycleSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if s.dead.Load() {
		return nil, s.byeErr()
	}
	return s.Session.Select(ctx, mailbox, options)
}

func (s *notifyByeLifecycleSession) Expunge(ctx context.Context, w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if s.dead.Load() {
		return s.byeErr()
	}
	return s.Session.Expunge(ctx, w, uids)
}

func (s *notifyByeLifecycleSession) Unselect(ctx context.Context) error {
	if s.dead.Load() && !s.allowUnselectWhenDead {
		return s.byeErr()
	}
	return s.Session.Unselect(ctx)
}

func (s *notifyByeLifecycleSession) Unauthenticate(ctx context.Context) error {
	if s.dead.Load() {
		return s.byeErr()
	}
	if u, ok := s.Session.(imapserver.SessionUnauthenticate); ok {
		return u.Unauthenticate(ctx)
	}
	return errors.New("unsupported")
}

func (s *notifyByeLifecycleSession) SetNotify(ctx context.Context, options *imap.NotifyOptions, w *imapserver.UpdateWriter) error {
	if s.dead.Load() {
		return s.byeErr()
	}
	if n, ok := s.Session.(imapserver.SessionNotify); ok {
		return n.SetNotify(ctx, options, w)
	}
	return nil
}

func (s *notifyByeLifecycleSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.dead.Load() {
		s.postByePollCalls.Add(1)
		if w.CondStoreEnabled() {
			s.postByeSawCondStore.Store(true)
		}
		if s.emitModSeqInPostByeWindow {
			_ = w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 1)
		}
		if s.failNotifyPollWhenDead {
			return s.byeErr()
		}
	} else {
		select {
		case s.pollStarted <- struct{}{}:
		default:
		}
	}
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}

func TestByeWithActiveNotifyDoesNotRestartPumpDropByeOrLeakCondStore(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		needSelected          bool
		allowUnselectWhenDead bool
		cmd                   string
		checkCondStore        bool
	}{
		{
			name:           "SELECT_CONDSTORE_from_authenticated",
			needSelected:   false,
			cmd:            "SELECT Other (CONDSTORE)",
			checkCondStore: true,
		},
		{
			name:           "SELECT_CONDSTORE_from_selected_Unselect_BYE",
			needSelected:   true,
			cmd:            "SELECT Other (CONDSTORE)",
			checkCondStore: true,
		},
		{
			name:                  "SELECT_CONDSTORE_from_selected_Select_BYE",
			needSelected:          true,
			allowUnselectWhenDead: true,
			cmd:                   "SELECT Other (CONDSTORE)",
			checkCondStore:        true,
		},
		{
			name:           "NOTIFY_SET_MODSEQ_BYE",
			needSelected:   true,
			cmd:            "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge))",
			checkCondStore: true,
		},
		{
			name:         "UNSELECT_BYE",
			needSelected: true,
			cmd:          "UNSELECT",
		},
		{
			name:         "CLOSE_Expunge_BYE",
			needSelected: true,
			cmd:          "CLOSE",
		},
		{
			name:         "UNAUTHENTICATE_BYE",
			needSelected: false,
			cmd:          "UNAUTHENTICATE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := imapmemserver.New()
			user := imapmemserver.NewUser("user", "pass")
			for _, box := range []string{"INBOX", "Other"} {
				if err := user.Create(context.Background(), box, nil); err != nil {
					t.Fatal(err)
				}
			}
			mem.AddUser(user)

			var sess *notifyByeLifecycleSession
			srv := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					sess = &notifyByeLifecycleSession{
						Session:                   mem.NewSession(),
						allowUnselectWhenDead:     tc.allowUnselectWhenDead,
						pollStarted:               make(chan struct{}, 1),
						failNotifyPollWhenDead:    true,
						emitModSeqInPostByeWindow: tc.needSelected && !tc.allowUnselectWhenDead && tc.checkCondStore,
					}
					return sess, nil, nil
				},
				Caps: imap.CapSet{
					imap.CapIMAP4rev1:      {},
					imap.CapCondStore:      {},
					imap.CapNotify:         {},
					imap.CapUnselect:       {},
					imap.CapUnauthenticate: {},
				},
				InsecureAuth: true,
				Logger:       log.New(io.Discard, "", 0),
			})
			t.Cleanup(func() { srv.Close() })

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(ln)

			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

			c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
			if _, err := c.br.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			c.do("login", "LOGIN user pass")
			if tc.needSelected {
				c.appendMessage("msg1")
				c.do("sel0", "SELECT INBOX")
				c.do("n0", "NOTIFY SET (SELECTED (MessageNew (UID FLAGS) MessageExpunge FlagChange))")
			} else {
				c.do("n0", "NOTIFY SET STATUS (MAILBOXES Other (MessageNew MessageExpunge))")
			}
			<-sess.pollStarted

			sess.dead.Store(true)
			if _, err := fmt.Fprintf(c.conn, "b1 %s\r\n", tc.cmd); err != nil {
				t.Fatal(err)
			}

			raw, _ := io.ReadAll(c.br)
			resp := string(raw)

			if sess.postByePollCalls.Load() > 0 {
				t.Errorf("handler restarted NotifyPoll %d time(s) after backend already returned BYE (wire=%q)",
					sess.postByePollCalls.Load(), resp)
			}
			if tc.checkCondStore && sess.postByeSawCondStore.Load() {
				t.Errorf("restarted NotifyPoll observed CondStoreEnabled()==true after BYE (wire=%q)", resp)
			}
			if tc.checkCondStore && strings.Contains(resp, "MODSEQ (") {
				t.Errorf("restarted NotifyPoll leaked MODSEQ fetch item on wire to non-CONDSTORE client after BYE: %q", resp)
			}
			if !strings.Contains(resp, "* BYE Session terminated by backend") {
				t.Errorf("client never received untagged * BYE: got %q", resp)
			}
		})
	}
}
