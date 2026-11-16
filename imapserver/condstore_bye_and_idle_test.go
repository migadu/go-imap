package imapserver_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
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
