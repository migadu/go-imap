package imapserver_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// byeSession is a real in-memory session that can be told to hand back a
// BYE-typed error from Select or from Idle — the two ways a backend says "this
// connection is over" (the mailbox stopped being serveable underneath it: a
// UIDVALIDITY change, a deletion).
type byeSession struct {
	*imapmemserver.UserSession
	selectBye bool
	pollBye   bool
	idleBye   chan struct{} // close to make a running Idle return the BYE
	idleFail  chan struct{} // close to make a running Idle return a NON-BYE error

	// idleByeAtOnce makes Idle return the BYE synchronously, before it ever
	// blocks — the backend already knew the mailbox was gone when IDLE came in.
	idleByeAtOnce bool
}

func newByeSession(user *imapmemserver.User) *byeSession {
	return &byeSession{
		UserSession: imapmemserver.NewUserSession(user),
		idleBye:     make(chan struct{}),
		idleFail:    make(chan struct{}),
	}
}

// errIdleBroke is the shape of an ordinary backend failure mid-IDLE — a write
// error, a lost upstream — that is NOT a BYE and so does not end the connection.
var errIdleBroke = errors.New("backend broke mid-idle")

func byeErr() error {
	return &imap.Error{Type: imap.StatusResponseTypeBye, Text: "mailbox state changed"}
}

func (s *byeSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if s.selectBye {
		return nil, byeErr()
	}
	return s.UserSession.Select(ctx, mailbox, options)
}

func (s *byeSession) Poll(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.pollBye {
		return byeErr()
	}
	return s.UserSession.Poll(ctx, w, allowExpunge)
}

func (s *byeSession) Idle(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if s.idleByeAtOnce {
		return byeErr()
	}
	select {
	case <-s.idleBye:
		return byeErr()
	case <-s.idleFail:
		return errIdleBroke
	case <-stop:
		return nil
	case <-ctx.Done():
		return nil
	}
}

// readUntilClosed reads lines until the server closes the connection, failing
// the test if it does not within the deadline.
func readUntilClosed(t *testing.T, wc *wireConn) []string {
	t.Helper()
	var lines []string
	for i := 0; i < 8; i++ {
		line, err := wc.readLine()
		if err != nil {
			return lines
		}
		lines = append(lines, line)
	}
	t.Fatalf("connection stayed open; read %q", lines)
	return nil
}

// TestByeErrorFromHandlerIsUntagged pins how a BYE-typed error returned by a
// command handler reaches the client.
//
// BYE is untagged by definition (RFC 9051 §7.1.5), so it cannot double as a
// command's tagged completion. Folding it into the tagged line produced
// `A2 BYE …` — not a valid tagged response — and, because that read as an
// ordinary completion, left the connection open on a session that then refused
// every command. NewSession's BYE already had this right; the command path did
// not.
func TestByeErrorFromHandlerIsUntagged(t *testing.T) {
	user := newTestUser(t)
	srv := newShutdownTestServer(t, func() imapserver.Session {
		s := newByeSession(user)
		s.selectBye = true
		return s
	})

	wc := dialWire(t, srv.addr)
	wc.login(t)

	tag := wc.send(t, "SELECT INBOX")
	lines := readUntilClosed(t, wc)

	if len(lines) == 0 {
		t.Fatal("server closed the connection without a BYE")
	}
	if got := lines[0]; got != "* BYE mailbox state changed" {
		t.Errorf("first line = %q, want an untagged BYE", got)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, tag+" ") {
			t.Errorf("BYE was written as the tagged response %q", l)
		}
	}
}

// TestByeFromCommandBoundaryPollIsUntagged covers the OTHER way a session
// reports it is finished: not from the command handler, but from the poll that
// flushes pending updates just before the tagged completion. That is where a
// backend most often notices (it is the only place a quiet session looks at its
// mailbox at all), and its error takes an early return that skips the handler
// error mapping — so the BYE reached the client as a bare EOF, with no BYE and
// no tagged response to the command it had in flight.
func TestByeFromCommandBoundaryPollIsUntagged(t *testing.T) {
	user := newTestUser(t)
	srv := newShutdownTestServer(t, func() imapserver.Session {
		s := newByeSession(user)
		s.pollBye = true
		return s
	})

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX") // Poll runs at the NEXT command boundary

	tag := wc.send(t, "NOOP")
	lines := readUntilClosed(t, wc)

	if len(lines) == 0 {
		t.Fatal("server closed the connection without a BYE")
	}
	if got := lines[0]; got != "* BYE mailbox state changed" {
		t.Errorf("first line = %q, want an untagged BYE", got)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, tag+" ") {
			t.Errorf("BYE was written as the tagged response %q", l)
		}
	}
}

// TestIdleEndedByBackendDisconnectsPromptly pins that a backend can end an IDLE
// on its own.
//
// handleIdle parks on the socket read that waits for DONE, so the backend's
// return was not even looked at until the client re-issued IDLE — up to 29
// minutes later (RFC 2177 §3) — and a backend that needed to disconnect sooner
// had to reach around the library and close the socket itself.
func TestIdleEndedByBackendDisconnectsPromptly(t *testing.T) {
	user := newTestUser(t)
	var mu sync.Mutex
	var sessions []*byeSession
	srv := newShutdownTestServer(t, func() imapserver.Session {
		s := newByeSession(user)
		mu.Lock()
		sessions = append(sessions, s)
		mu.Unlock()
		return s
	}, imap.CapIdle)

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX")

	tag := wc.send(t, "IDLE")
	if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE = %q, %v; want a continuation request", line, err)
	}

	mu.Lock()
	s := sessions[0]
	mu.Unlock()
	start := time.Now()
	close(s.idleBye) // the backend gives up on this mailbox

	// Deliberately no DONE: everything below must arrive unprompted.
	lines := readUntilClosed(t, wc)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v to end the IDLE; it must not wait for DONE", elapsed)
	}
	if len(lines) == 0 {
		t.Fatal("server closed the connection without a BYE")
	}
	if got := lines[0]; got != "* BYE mailbox state changed" {
		t.Errorf("first line = %q, want an untagged BYE", got)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, tag+" ") {
			t.Errorf("IDLE was answered with the tagged response %q", l)
		}
	}
}

// TestIdleByeBeforeTheWaitBeginsDisconnectsPromptly is the same promise for a
// backend that fails the moment it is asked to idle — the mailbox was already
// gone when IDLE arrived, so Idle returns the BYE before it ever blocks.
//
// That is the ordering the interrupt is most exposed to: the interrupt is a
// read deadline in the past, and if the idle deadline were armed AFTER the
// watcher started, an interrupt that had already landed would be overwritten
// by the arm and the wait for DONE would block on the full idle window as if
// the backend had said nothing. handleIdle arms the deadline before either
// goroutine exists for exactly this reason; the case is timing-dependent, so
// this pins the promise rather than reliably reproducing the loss.
func TestIdleByeBeforeTheWaitBeginsDisconnectsPromptly(t *testing.T) {
	user := newTestUser(t)
	srv := newShutdownTestServer(t, func() imapserver.Session {
		s := newByeSession(user)
		s.idleByeAtOnce = true
		return s
	}, imap.CapIdle)

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX")

	tag := wc.send(t, "IDLE")
	start := time.Now()
	// The continuation request may or may not precede the BYE: the backend
	// goroutine is started only after "+ idling" is written, so it always does
	// here — but nothing below depends on it, and a client must cope with
	// either order.
	lines := readUntilClosed(t, wc)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v to end the IDLE; it must not wait for DONE", elapsed)
	}
	var sawBye bool
	for _, l := range lines {
		if l == "* BYE mailbox state changed" {
			sawBye = true
		}
		if strings.HasPrefix(l, tag+" ") {
			t.Errorf("IDLE was answered with the tagged response %q", l)
		}
	}
	if !sawBye {
		t.Fatalf("server closed the connection without a BYE; read %q", lines)
	}
}

// TestIdleNonByeBackendErrorWaitsForDone pins the boundary of the interrupt:
// only a BYE ends the wait for DONE early. Any other error a backend returns
// mid-IDLE is held until the client's DONE and becomes the tagged NO that
// answers it — as it always was — because a tagged completion delivered while
// the client still owes a DONE is a line the client is not expecting, and the
// DONE it then sends is a line the server cannot parse as a command. A BYE has
// no such follow-up: it ends the connection.
func TestIdleNonByeBackendErrorWaitsForDone(t *testing.T) {
	user := newTestUser(t)
	var mu sync.Mutex
	var sessions []*byeSession
	srv := newShutdownTestServer(t, func() imapserver.Session {
		s := newByeSession(user)
		mu.Lock()
		sessions = append(sessions, s)
		mu.Unlock()
		return s
	}, imap.CapIdle)

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX")

	tag := wc.send(t, "IDLE")
	if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE = %q, %v; want a continuation request", line, err)
	}

	mu.Lock()
	s := sessions[0]
	mu.Unlock()
	close(s.idleFail) // an ordinary failure, not a BYE

	// Nothing may arrive while the client is still parked: no tagged line, no
	// BYE, no close. A bounded read that times out is the pass condition.
	wc.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if line, err := wc.br.ReadString('\n'); err == nil {
		t.Fatalf("server spoke before DONE on a non-BYE backend error: %q", strings.TrimRight(line, "\r\n"))
	} else if !isTimeout(err) {
		t.Fatalf("read before DONE = %v; want a timeout (silence)", err)
	}

	// DONE is answered with the backend's failure as a tagged NO ...
	if _, err := wc.conn.Write([]byte("DONE\r\n")); err != nil {
		t.Fatalf("writing DONE: %v", err)
	}
	lines, err := wc.waitTagged(tag)
	if err != nil {
		t.Fatalf("after DONE: %v (read %q)", err, lines)
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" NO") {
		t.Errorf("IDLE completion = %q, want %s NO", last, tag)
	}
	// ... and the connection is still a working session afterwards.
	wc.mustOK(t, "NOOP")
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// idleTimeoutListener imposes a per-read inactivity deadline on every accepted
// connection, refreshed on read progress — the shape of a connection guard
// sitting in front of the server (and of the deployment that found this bug,
// where the guard's window was shorter than idleReadTimeout, so it always won).
type idleTimeoutListener struct {
	net.Listener
	idle time.Duration
}

func (l idleTimeoutListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &idleTimeoutConn{Conn: c, idle: l.idle}, nil
}

type idleTimeoutConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

// TestIdleReadTimeoutSendsBye pins what a client parked in IDLE is told when the
// read deadline expires.
//
// It is an inactivity timeout, which RFC 9051 §5.4 says to announce with an
// untagged BYE. The raw error was instead mapped to the catch-all
// `NO [SERVERBUG] Internal server error` — telling the client its perfectly
// ordinary IDLE had hit a server malfunction, on a connection the serve loop
// then kept reading — and logged every such disconnect as a command failure.
func TestIdleReadTimeoutSendsBye(t *testing.T) {
	user := newTestUser(t)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return newByeSession(user), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapIdle: {}},
	})
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go server.Serve(idleTimeoutListener{Listener: ln, idle: 500 * time.Millisecond})
	t.Cleanup(func() { server.Close() })

	wc := dialWire(t, ln.Addr().String())
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX")

	tag := wc.send(t, "IDLE")
	if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE = %q, %v; want a continuation request", line, err)
	}

	lines := readUntilClosed(t, wc)
	if len(lines) == 0 {
		t.Fatal("server closed the connection without a BYE")
	}
	if got := lines[0]; !strings.HasPrefix(got, "* BYE") {
		t.Errorf("first line = %q, want an untagged BYE", got)
	}
	for _, l := range lines {
		if strings.Contains(l, "SERVERBUG") {
			t.Errorf("an idle timeout was reported as a server bug: %q", l)
		}
		if strings.HasPrefix(l, tag+" ") {
			t.Errorf("IDLE was answered with the tagged response %q", l)
		}
	}
}
