package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type taggedBoundarySession struct {
	imapserver.Session
	gate                               atomic.Bool
	entered, release, trigger, written chan struct{}
}

func (s *taggedBoundarySession) HighestModSeq() uint64 {
	if s.gate.Swap(false) {
		close(s.entered)
		<-s.release
	}
	return 3
}
func (s *taggedBoundarySession) SetNotify(context.Context, *imap.NotifyOptions, *imapserver.UpdateWriter) error {
	return nil
}
func (s *taggedBoundarySession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return nil
	case <-stop:
		return nil
	case <-s.trigger:
	}
	if err := w.WriteMessageFlags(2, 2, []imap.Flag{imap.FlagFlagged}, 6); err != nil {
		return err
	}
	close(s.written)
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}
func TestHighestModSeqNotifyTaggedBoundary(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	sessions := make(chan *taggedBoundarySession, 1)
	srv := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		s := &taggedBoundarySession{Session: mem.NewSession(), entered: make(chan struct{}), release: make(chan struct{}), trigger: make(chan struct{}), written: make(chan struct{})}
		sessions <- s
		return s, nil, nil
	}, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}, imap.CapEnable: {}, imap.CapNotify: {}}, InsecureAuth: true})
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
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn), addr: ln.Addr().String()}
	c.br.ReadString('\n')
	s := <-sessions
	var release sync.Once
	defer release.Do(func() { close(s.release) })
	c.do("login", "LOGIN user pass")
	c.appendMessage("append1")
	c.appendMessage("append2")
	c.do("enable", "ENABLE CONDSTORE")
	c.do("select", "SELECT INBOX")
	c.do("notify", "NOTIFY SET (SELECTED (MessageNew MessageExpunge FlagChange))")
	s.gate.Store(true)
	if _, err := fmt.Fprintf(conn, "fetch FETCH 1 (FLAGS MODSEQ)\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("getter not reached")
	}
	close(s.trigger)
	// Before the fix the pump writes while HighestModSeq is gated, after
	// finishCondStore took its snapshot. With one encoder span it blocks
	// until the tagged completion; its item then belongs to the next span.
	select {
	case <-s.written:
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(s.release) })
	var b strings.Builder
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		if strings.HasPrefix(line, "fetch ") {
			break
		}
	}
	resp := b.String()
	if !strings.Contains(resp, "MODSEQ (6)") {
		// The pump was serialized after this tag. Its MODSEQ must be retained
		// until the next tagged response, rather than erased by a deferred reset.
		select {
		case <-s.written:
		case <-time.After(5 * time.Second):
			t.Fatal("notification did not complete after the tagged response")
		}
		resp = c.do("next", "NOOP")
	}
	if !strings.Contains(resp, "MODSEQ (6)") {
		t.Fatalf("pump MODSEQ missing: %q", resp)
	}
	if got := highestModSeqLines(resp); len(got) != 1 || !strings.Contains(got[0], "HIGHESTMODSEQ 3]") {
		t.Fatalf("client sees MODSEQ6 above held gap4 but no safe HIGHEST before tag: %q", resp)
	}
}
