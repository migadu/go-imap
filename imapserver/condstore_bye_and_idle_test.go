package imapserver_test

import (
	"bufio"
	"context"
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

func TestOtherByePathsFlushClosingWatermark(t *testing.T) {
	t.Run("Desynchronized_BYE", func(t *testing.T) {
		c, _, s := newBadAndByeCondStoreConn(t, false, 3, true, imap.CapQResync, imap.CapNotify)
		c.appendMessage("append2")
		c.do("enable", "ENABLE QRESYNC")
		c.do("select", "SELECT INBOX")
		c.do("del", `STORE 2 +FLAGS.SILENT (\Deleted)`)
		c.do("notify", "NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge FlagChange))")

		close(s.trigger)
		<-s.written

		if _, err := fmt.Fprint(c.conn, "x1 STORE 1 UNKNOWN {5+}\r\nhello\r\n"); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(c.br)
		if err != nil {
			t.Fatal(err)
		}
		resp := string(raw)
		byeIdx := strings.Index(resp, "* BYE")
		if byeIdx < 0 {
			t.Fatalf("missing * BYE in desync output:\n%s", resp)
		}
		if !strings.Contains(resp[:byeIdx], "* OK [HIGHESTMODSEQ 3]") {
			t.Fatalf("Desynchronized BYE leaked MODSEQ (10) before * BYE without compensating * OK [HIGHESTMODSEQ 3]:\n%s", resp)
		}
	})

	t.Run("ErrMaxSize_BYE_in_tag", func(t *testing.T) {
		c, _, s := newBadAndByeCondStoreConn(t, false, 3, true, imap.CapQResync, imap.CapNotify)
		c.appendMessage("append2")
		c.do("enable", "ENABLE QRESYNC")
		c.do("select", "SELECT INBOX")
		c.do("del", `STORE 2 +FLAGS.SILENT (\Deleted)`)
		c.do("notify", "NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge FlagChange))")

		close(s.trigger)
		<-s.written

		oversized := strings.Repeat("A", 51*1024) + " NOOP\r\n"
		if _, err := io.WriteString(c.conn, oversized); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(c.br)
		if err != nil {
			t.Fatal(err)
		}
		resp := string(raw)
		byeIdx := strings.Index(resp, "* BYE")
		if byeIdx < 0 {
			t.Fatalf("missing * BYE in ErrMaxSize output:\n%s", resp)
		}
		if !strings.Contains(resp[:byeIdx], "* OK [HIGHESTMODSEQ 3]") {
			t.Fatalf("ErrMaxSize BYE leaked MODSEQ (10) before * BYE without compensating * OK [HIGHESTMODSEQ 3]:\n%s", resp)
		}
	})
}

type idleStopFlushSession struct {
	imapserver.Session
	watermark uint64
}

func (s *idleStopFlushSession) HighestModSeq() uint64 {
	return s.watermark
}

func (s *idleStopFlushSession) Idle(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	_ = w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 8)
	<-stop
	time.Sleep(25 * time.Millisecond)
	_ = w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagFlagged}, 10)
	return nil
}

func TestIdleTimeoutByeAwaitsBackend(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &idleStopFlushSession{
				Session:   mem.NewSession(),
				watermark: 3,
			}, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapCondStore: {},
			imap.CapQResync:   {},
			imap.CapEnable:    {},
			imap.CapIdle:      {},
		},
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
	})
	t.Cleanup(func() { srv.Close() })

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := authenticateDeadlineListener{rawLn}
	go srv.Serve(ln)

	wc := dialWire(t, ln.Addr().String())
	wc.mustOK(t, "LOGIN user pass")
	wc.mustOK(t, "APPEND INBOX {4}\r\nmsg1")
	wc.mustOK(t, "APPEND INBOX {4}\r\nmsg2")
	wc.mustOK(t, "ENABLE QRESYNC")
	wc.mustOK(t, "SELECT INBOX")

	_ = wc.send(t, "IDLE")
	if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "+ ") {
		t.Fatalf("IDLE continuation = %q, %v", line, err)
	}
	raw, err := io.ReadAll(wc.br)
	if err != nil {
		t.Fatal(err)
	}
	resp := string(raw)
	byeIdx := strings.Index(resp, "* BYE Idle timeout")
	if byeIdx < 0 {
		t.Fatalf("missing * BYE Idle timeout:\n%s", resp)
	}
	afterBye := strings.TrimSpace(resp[byeIdx+len("* BYE Idle timeout"):])
	if afterBye != "" {
		t.Fatalf("server wrote responses AFTER * BYE Idle timeout because handleIdle did not awaitBackend():\n%s", resp)
	}
	preBye := resp[:byeIdx]
	mod10Idx := strings.Index(preBye, "MODSEQ (10)")
	wmIdx := strings.LastIndex(preBye, "* OK [HIGHESTMODSEQ 3]")
	if mod10Idx >= 0 && wmIdx < mod10Idx {
		t.Fatalf("idle timeout BYE leaked MODSEQ (10) after flushClosingCondStore() without compensating * OK [HIGHESTMODSEQ 3]:\n%s", resp)
	}
}

type selectAndNotifyBadCondStoreSession struct {
	imapserver.Session
	selectBad           atomic.Bool
	notifyBad           atomic.Bool
	pollRuns            atomic.Int32
	sawEnabledOnStop    atomic.Bool
	sawEnabledOnRestart atomic.Bool
}

func (s *selectAndNotifyBadCondStoreSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if s.selectBad.Load() {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Invalid SELECT option for backend",
		}
	}
	return s.Session.Select(ctx, mailbox, options)
}

func (s *selectAndNotifyBadCondStoreSession) SetNotify(context.Context, *imap.NotifyOptions, *imapserver.UpdateWriter) error {
	if s.notifyBad.Load() {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Invalid NOTIFY option for backend",
		}
	}
	return nil
}

func (s *selectAndNotifyBadCondStoreSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	run := s.pollRuns.Add(1)
	if run > 1 && w.CondStoreEnabled() {
		s.sawEnabledOnRestart.Store(true)
	}
	select {
	case <-ctx.Done():
	case <-stop:
		if w.CondStoreEnabled() {
			s.sawEnabledOnStop.Store(true)
		}
	}
	return nil
}

func TestSelectAndNotifyBadDoNotLeakCondStoreToNotifyPump(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	for _, box := range []string{"INBOX", "Other"} {
		if err := user.Create(context.Background(), box, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)

	var sess *selectAndNotifyBadCondStoreSession
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			sess = &selectAndNotifyBadCondStoreSession{Session: mem.NewSession()}
			return sess, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapCondStore: {},
			imap.CapNotify:    {},
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
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	c.do("login", "LOGIN user pass")
	c.do("notify0", "NOTIFY SET STATUS (MAILBOXES Other (MessageNew MessageExpunge))")

	// Wait briefly so the initial NotifyPoll goroutine is parked on <-stop.
	for sess.pollRuns.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	sess.notifyBad.Store(true)
	resp := c.do("n1", "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge))")
	if !strings.Contains(resp, "n1 BAD") {
		t.Fatalf("expected BAD from SetNotify, got %q", resp)
	}
	if sess.sawEnabledOnStop.Swap(false) {
		t.Errorf("handleNotify committed CONDSTORE before stopping existing NotifyPoll")
	}

	sess.selectBad.Store(true)
	for i := 0; i < 40; i++ {
		tag := fmt.Sprintf("s%d", i)
		sResp := c.do(tag, "SELECT Other (CONDSTORE)")
		if !strings.Contains(sResp, tag+" BAD") {
			t.Fatalf("expected BAD from Select, got %q", sResp)
		}
	}
	if sess.sawEnabledOnStop.Load() {
		t.Errorf("handleSelect committed CONDSTORE before fencing existing NotifyPoll")
	}
	if sess.sawEnabledOnRestart.Load() {
		t.Errorf("handleSelect restarted NotifyPoll with uncommitted CONDSTORE still enabled after BAD")
	}
}

type idlePostStopByeSession struct {
	imapserver.Session
	watermark uint64
}

func (s *idlePostStopByeSession) HighestModSeq() uint64 {
	return s.watermark
}

func (s *idlePostStopByeSession) Idle(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	_ = w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10)
	<-stop
	return &imap.Error{
		Type: imap.StatusResponseTypeBye,
		Text: "Mailbox lost on idle stop",
	}
}

func TestIdleBackendByeOnStopNotSwallowedByInvalidDone(t *testing.T) {
	for i := 0; i < 20; i++ {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser("user", "pass")
		if err := user.Create(context.Background(), "INBOX", nil); err != nil {
			t.Fatal(err)
		}
		mem.AddUser(user)

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &idlePostStopByeSession{
					Session:   mem.NewSession(),
					watermark: 3,
				}, nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1: {},
				imap.CapCondStore: {},
				imap.CapQResync:   {},
				imap.CapEnable:    {},
				imap.CapIdle:      {},
			},
			InsecureAuth: true,
			Logger:       log.New(io.Discard, "", 0),
		})

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		go srv.Serve(ln)

		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
		_, _ = c.br.ReadString('\n')
		c.do("login", "LOGIN user pass")
		c.appendMessage("msg1")
		c.appendMessage("msg2")
		c.do("enable", "ENABLE QRESYNC")
		c.do("select", "SELECT INBOX")

		_, _ = fmt.Fprint(c.conn, "id1 IDLE\r\n")
		line, err := c.br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "+ ") {
			conn.Close()
			srv.Close()
			t.Fatalf("expected IDLE continuation, got %q, %v", line, err)
		}
		_, _ = fmt.Fprint(c.conn, "NOTDONE\r\n")

		raw, _ := io.ReadAll(c.br)
		conn.Close()
		srv.Close()

		resp := string(raw)
		byeIdx := strings.Index(resp, "* BYE Mailbox lost on idle stop")
		if byeIdx < 0 {
			t.Fatalf("iter %d: handleIdle swallowed backend * BYE error on stop:\n%s", i, resp)
		}
		if !strings.Contains(resp[:byeIdx], "* OK [HIGHESTMODSEQ 3]") {
			t.Fatalf("iter %d: missing compensating * OK [HIGHESTMODSEQ 3] before backend * BYE:\n%s", i, resp)
		}
	}
}
