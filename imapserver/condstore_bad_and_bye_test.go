package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type badAndByeCondStoreSession struct {
	imapserver.Session
	watermark atomic.Uint64
	fired     atomic.Bool
	byeOnPoll bool
	trigger   chan struct{}
	written   chan struct{}
}

func (s *badAndByeCondStoreSession) HighestModSeq() uint64 {
	return s.watermark.Load()
}

func (s *badAndByeCondStoreSession) SetNotify(ctx context.Context, options *imap.NotifyOptions, w *imapserver.UpdateWriter) error {
	if sn, ok := s.Session.(imapserver.SessionNotify); ok && s.trigger == nil {
		return sn.SetNotify(ctx, options, w)
	}
	return nil
}

func (s *badAndByeCondStoreSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.trigger == nil {
		return s.Session.(imapserver.SessionNotify).NotifyPoll(ctx, w, stop)
	}
	if !s.fired.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
		case <-stop:
		}
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-stop:
		return nil
	case <-s.trigger:
	}
	if err := w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
		return err
	}
	close(s.written)
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}

func (s *badAndByeCondStoreSession) Poll(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.byeOnPoll && s.fired.Load() {
		return &imap.Error{
			Type: imap.StatusResponseTypeBye,
			Text: "Session expired",
		}
	}
	return s.Session.Poll(ctx, w, allowExpunge)
}

func (s *badAndByeCondStoreSession) Thread(ctx context.Context, numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	return s.Session.(imapserver.SessionThread).Thread(ctx, numKind, algorithm, charset, criteria)
}

func newBadAndByeCondStoreConn(t *testing.T, byeOnPoll bool, watermark uint64, withTrigger bool, extraCaps ...imap.Cap) (*storeTestConn, *imapserver.Server, *badAndByeCondStoreSession) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)

	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapCondStore: {},
		imap.CapEnable:    {},
	}
	for _, cap := range extraCaps {
		caps[cap] = struct{}{}
	}

	conns := make(chan *imapserver.Conn, 1)
	sessions := make(chan *badAndByeCondStoreSession, 1)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			conns <- conn
			s := &badAndByeCondStoreSession{
				Session:   mem.NewSession(),
				byeOnPoll: byeOnPoll,
			}
			s.watermark.Store(watermark)
			if withTrigger {
				s.trigger = make(chan struct{})
				s.written = make(chan struct{})
			}
			sessions <- s
			return s, nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
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
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	c.srvConn = <-conns
	s := <-sessions
	c.do("login", "LOGIN user pass")
	c.appendMessage("append1")
	return c, srv, s
}

func TestBadResponseAfterCommitCondStoreDoesNotEnableCondStore(t *testing.T) {
	c, _, _ := newBadAndByeCondStoreConn(t, false, 4242, false, imap.Cap("THREAD=REFERENCES"))
	c.do("select", "SELECT INBOX")

	resp := c.do("th", "THREAD REFS UTF-8 MODSEQ 1")
	if !strings.Contains(resp, "th BAD") {
		t.Fatalf("expected th BAD, got: %q", resp)
	}
	if strings.Contains(resp, "[HIGHESTMODSEQ") {
		t.Errorf("command answered BAD emitted first-enable HIGHESTMODSEQ: %q", resp)
	}
	if c.srvConn.CondStoreEnabled() {
		t.Errorf("CondStoreEnabled() = true after command answered BAD")
	}

	storeResp := c.do("store", `STORE 1 +FLAGS (\Seen)`)
	if strings.Contains(storeResp, "MODSEQ (") {
		t.Errorf("command answered BAD latched CONDSTORE and leaked unrequested MODSEQ on STORE (RFC 7162 §3.2): %q", storeResp)
	}
}

func TestByePathsFlushDelayedClosingWatermark(t *testing.T) {
	t.Run("Server_Shutdown", func(t *testing.T) {
		c, srv, s := newBadAndByeCondStoreConn(t, false, 3, true, imap.CapQResync, imap.CapNotify)
		c.appendMessage("append2")
		c.do("enable", "ENABLE QRESYNC")
		c.do("select", "SELECT INBOX")
		c.do("del", `STORE 2 +FLAGS.SILENT (\Deleted)`)
		c.do("notify", "NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge FlagChange))")

		close(s.trigger)
		<-s.written

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		go func() { _ = srv.Shutdown(ctx) }()

		raw, err := io.ReadAll(c.br)
		if err != nil {
			t.Fatal(err)
		}
		resp := string(raw)
		if !strings.Contains(resp, "* BYE") {
			t.Fatalf("missing * BYE in shutdown output:\n%s", resp)
		}
		byeIdx := strings.Index(resp, "* BYE")
		if !strings.Contains(resp[:byeIdx], "* OK [HIGHESTMODSEQ 3]") {
			t.Errorf("Shutdown leaked MODSEQ (10) before * BYE without compensating * OK [HIGHESTMODSEQ 3] (unlike LOGOUT):\n%s", resp)
		}
	})

	t.Run("finishBye_on_backend_or_timeout_BYE", func(t *testing.T) {
		c, _, s := newBadAndByeCondStoreConn(t, true, 3, true, imap.CapQResync, imap.CapNotify)
		c.appendMessage("append2")
		c.do("enable", "ENABLE QRESYNC")
		c.do("select", "SELECT INBOX")
		c.do("del", `STORE 2 +FLAGS.SILENT (\Deleted)`)
		c.do("notify", "NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge FlagChange))")

		close(s.trigger)
		<-s.written

		if _, err := fmt.Fprint(c.conn, "n1 NOOP\r\n"); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(c.br)
		if err != nil {
			t.Fatal(err)
		}
		resp := string(raw)
		if !strings.Contains(resp, "* BYE Session expired") {
			t.Fatalf("missing * BYE in finishBye output:\n%s", resp)
		}
		byeIdx := strings.Index(resp, "* BYE")
		if !strings.Contains(resp[:byeIdx], "* OK [HIGHESTMODSEQ 3]") {
			t.Errorf("finishBye leaked MODSEQ (10) before * BYE without compensating * OK [HIGHESTMODSEQ 3] (unlike LOGOUT):\n%s", resp)
		}
	})
}
