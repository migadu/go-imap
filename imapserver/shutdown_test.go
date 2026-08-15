package imapserver_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// These tests speak IMAP over a raw socket rather than through imapclient, so
// they can assert the exact lines the server writes and the order they arrive
// in. Graceful shutdown is all about that ordering.
//
// Clients here behave like real ones on BYE: they read to EOF and close their
// side. That is what lets Shutdown return promptly, and the tests assert that
// it does.

const shutdownBye = "* BYE Server shutting down"

// promptly is how long Shutdown may take when nothing is genuinely in flight.
// The real figure is milliseconds; the slack is for -race and loaded CI. It is
// well under every grace period used below, so a Shutdown that waited out the
// grace period cannot pass.
const promptly = time.Second

// gateSession embeds a real in-memory session and makes Select controllable:
// it blocks until released, then delegates. Leaving it unreleased models a
// backend that is genuinely stuck.
type gateSession struct {
	*imapmemserver.UserSession
	selectStarted chan struct{} // closed when Select is entered
	release       chan struct{} // close to let Select proceed
	ctxErr        chan error    // receives ctx.Err() if the context ends first
}

func newGateSession(user *imapmemserver.User) *gateSession {
	return &gateSession{
		UserSession:   imapmemserver.NewUserSession(user),
		selectStarted: make(chan struct{}),
		release:       make(chan struct{}),
		ctxErr:        make(chan error, 1),
	}
}

func (s *gateSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	close(s.selectStarted)
	select {
	case <-s.release:
		return s.UserSession.Select(ctx, mailbox, options)
	case <-ctx.Done():
		s.ctxErr <- ctx.Err()
		return nil, ctx.Err()
	}
}

// shutdownTestServer is a server with one user, INBOX created, listening on
// loopback.
type shutdownTestServer struct {
	server *imapserver.Server
	addr   string
}

func newShutdownTestServer(t *testing.T, newSession func() imapserver.Session, extraCaps ...imap.Cap) *shutdownTestServer {
	t.Helper()

	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
	for _, c := range extraCaps {
		caps[c] = struct{}{}
	}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return newSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         caps,
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })

	return &shutdownTestServer{server: server, addr: ln.Addr().String()}
}

func newTestUser(t *testing.T) *imapmemserver.User {
	t.Helper()
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	return user
}

// wireConn is a hand-driven IMAP client.
type wireConn struct {
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

func dialWire(t *testing.T, addr string) *wireConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	wc := &wireConn{conn: conn, br: bufio.NewReader(conn)}
	line, err := wc.readLine()
	if err != nil {
		t.Fatalf("reading greeting: %v", err)
	}
	if !strings.HasPrefix(line, "* OK") {
		t.Fatalf("greeting = %q, want * OK", line)
	}
	return wc
}

// readLine returns the next line without CRLF. Every read is bounded so a
// server that goes quiet fails the test instead of hanging it.
func (wc *wireConn) readLine() (string, error) {
	wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := wc.br.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading from server: %w (partial %q)", err, line)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// send writes a tagged command and returns its tag.
func (wc *wireConn) send(t *testing.T, cmd string) string {
	t.Helper()
	wc.tag++
	tag := fmt.Sprintf("A%d", wc.tag)
	if _, err := io.WriteString(wc.conn, tag+" "+cmd+"\r\n"); err != nil {
		t.Fatalf("writing %q: %v", cmd, err)
	}
	return tag
}

// waitTagged reads lines until the tagged completion for tag, returning every
// line read (the tagged one last).
func (wc *wireConn) waitTagged(tag string) ([]string, error) {
	var lines []string
	for {
		line, err := wc.readLine()
		if err != nil {
			return lines, err
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines, nil
		}
	}
}

func (wc *wireConn) mustOK(t *testing.T, cmd string) {
	t.Helper()
	tag := wc.send(t, cmd)
	lines, err := wc.waitTagged(tag)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK") {
		t.Fatalf("%s: got %q, want %s OK", cmd, last, tag)
	}
}

func (wc *wireConn) login(t *testing.T) {
	t.Helper()
	wc.mustOK(t, `LOGIN "user" "pass"`)
}

// awaitBye is what a client does when the server shuts down: it expects the
// very next line to be the BYE, then a clean EOF, and then closes its side. It
// returns an error describing the first deviation. Safe to call from a
// goroutine, which is how the tests use it: the client must be reading while
// Shutdown runs, or Shutdown would be waiting on a client that never answers.
func (wc *wireConn) awaitBye() error {
	line, err := wc.readLine()
	if err != nil {
		return fmt.Errorf("waiting for BYE: %w", err)
	}
	if line != shutdownBye {
		return fmt.Errorf("after shutdown got %q, want %q", line, shutdownBye)
	}
	wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := wc.br.ReadByte(); err != io.EOF {
		return fmt.Errorf("after BYE got %v, want a clean EOF", err)
	}
	return wc.conn.Close()
}

type shutdownResult struct {
	err     error
	elapsed time.Duration
}

// shutdownAsync runs Shutdown with the given grace period.
func shutdownAsync(server *imapserver.Server, grace time.Duration) <-chan shutdownResult {
	ch := make(chan shutdownResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		start := time.Now()
		err := server.Shutdown(ctx)
		ch <- shutdownResult{err: err, elapsed: time.Since(start)}
	}()
	return ch
}

func checkPrompt(t *testing.T, r shutdownResult, what string) {
	t.Helper()
	if r.err != nil {
		t.Errorf("Shutdown() = %v after %v, want nil: %s", r.err, r.elapsed.Round(time.Millisecond), what)
	}
	if r.elapsed > promptly {
		t.Errorf("Shutdown() took %v, want under %v: %s", r.elapsed.Round(time.Millisecond), promptly, what)
	}
}

// TestShutdownByesIdleConnections: a connection waiting for its next command
// has nothing in flight, so Shutdown must not wait for it. It gets BYE at once
// and Shutdown returns nil well inside the grace period.
//
// Before the fix nothing woke the idle read, so Shutdown burned the whole grace
// period and returned context.DeadlineExceeded on every routine restart.
func TestShutdownByesIdleConnections(t *testing.T) {
	user := newTestUser(t)
	srv := newShutdownTestServer(t, func() imapserver.Session { return imapmemserver.NewUserSession(user) })

	// One idle in every state a connection can be idle in.
	notAuthed := dialWire(t, srv.addr)
	authed := dialWire(t, srv.addr)
	authed.login(t)
	selected := dialWire(t, srv.addr)
	selected.login(t)
	selected.mustOK(t, "SELECT INBOX")

	conns := map[string]*wireConn{"not authenticated": notAuthed, "authenticated": authed, "selected": selected}
	byeErrs := make(chan error, len(conns))
	for name, wc := range conns {
		go func(name string, wc *wireConn) {
			if err := wc.awaitBye(); err != nil {
				byeErrs <- fmt.Errorf("%s connection: %w", name, err)
				return
			}
			byeErrs <- nil
		}(name, wc)
	}

	checkPrompt(t, <-shutdownAsync(srv.server, 5*time.Second), "only idle connections were open")
	for range conns {
		if err := <-byeErrs; err != nil {
			t.Error(err)
		}
	}
}

// TestShutdownWaitsForActiveCommand: a connection in the middle of a command
// is genuinely active. Shutdown must let the command finish, deliver its tagged
// completion, and only then send BYE -- and must not wait one moment longer.
func TestShutdownWaitsForActiveCommand(t *testing.T) {
	user := newTestUser(t)
	gate := newGateSession(user)
	srv := newShutdownTestServer(t, func() imapserver.Session { return gate })

	wc := dialWire(t, srv.addr)
	wc.login(t)
	tag := wc.send(t, "SELECT INBOX")
	select {
	case <-gate.selectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("SELECT never reached the backend")
	}

	// The client reads throughout: the command's responses, then BYE. A BYE
	// anywhere before the tagged line means the response stream was torn.
	clientErr := make(chan error, 1)
	go func() {
		lines, err := wc.waitTagged(tag)
		if err != nil {
			clientErr <- err
			return
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "* BYE") {
				clientErr <- fmt.Errorf("BYE arrived before the tagged completion; response stream torn:\n%s", strings.Join(lines, "\n"))
				return
			}
		}
		if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK") {
			clientErr <- fmt.Errorf("SELECT completion = %q, want %s OK", last, tag)
			return
		}
		clientErr <- wc.awaitBye()
	}()

	res := shutdownAsync(srv.server, 5*time.Second)

	// While the command is in flight Shutdown must be waiting, not done.
	select {
	case r := <-res:
		t.Fatalf("Shutdown() returned (%v) while a command was still running", r.err)
	case <-time.After(300 * time.Millisecond):
	}

	close(gate.release)

	r := <-res
	checkPrompt(t, r, "the command was released and finished")
	if err := <-clientErr; err != nil {
		t.Error(err)
	}
}

// TestShutdownEndsIdleCommand: an RFC 2177 IDLE has no work in flight either;
// the client is just parked. Shutdown ends it with BYE immediately rather than
// waiting up to the idle timeout for a DONE that is not coming.
func TestShutdownEndsIdleCommand(t *testing.T) {
	user := newTestUser(t)
	srv := newShutdownTestServer(t, func() imapserver.Session { return imapmemserver.NewUserSession(user) })

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX")
	tag := wc.send(t, "IDLE")
	line, err := wc.readLine()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "+ ") {
		t.Fatalf("IDLE continuation = %q, want +", line)
	}

	clientErr := make(chan error, 1)
	go func() {
		// No tagged completion for the IDLE: the server is ending the
		// session, and BYE is the whole of what it has to say.
		line, err := wc.readLine()
		if err != nil {
			clientErr <- err
			return
		}
		if strings.HasPrefix(line, tag+" ") {
			clientErr <- fmt.Errorf("got a tagged response %q to an IDLE ended by shutdown, want only %q", line, shutdownBye)
			return
		}
		if line != shutdownBye {
			clientErr <- fmt.Errorf("after shutdown got %q, want %q", line, shutdownBye)
			return
		}
		wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := wc.br.ReadByte(); err != io.EOF {
			clientErr <- fmt.Errorf("after BYE got %v, want a clean EOF", err)
			return
		}
		clientErr <- wc.conn.Close()
	}()

	checkPrompt(t, <-shutdownAsync(srv.server, 5*time.Second), "only an IDLE connection was open")
	if err := <-clientErr; err != nil {
		t.Error(err)
	}
}

// TestShutdownDoesNotWaitForClientClose: a client that reads the BYE but never
// closes its side must not hold Shutdown hostage. The lingering close is
// bounded, so Shutdown still returns nil well inside the grace period.
func TestShutdownDoesNotWaitForClientClose(t *testing.T) {
	user := newTestUser(t)
	srv := newShutdownTestServer(t, func() imapserver.Session { return imapmemserver.NewUserSession(user) })

	wc := dialWire(t, srv.addr)
	wc.login(t)
	// Deliberately no reader and no close: the connection just sits there.

	const grace = 5 * time.Second
	r := <-shutdownAsync(srv.server, grace)
	if r.err != nil {
		t.Errorf("Shutdown() = %v, want nil: the client not closing is not the server's problem", r.err)
	}
	if r.elapsed > grace/2 {
		t.Errorf("Shutdown() took %v, want well under the %v grace period: the linger must be bounded", r.elapsed.Round(time.Millisecond), grace)
	}

	// The BYE was still delivered, and the connection was closed cleanly.
	if err := wc.awaitBye(); err != nil {
		t.Error(err)
	}
}

// TestShutdownForceClosesStuckCommand is the guard for the backstop: a backend
// that never returns still gets force-closed when the grace period runs out,
// and Shutdown reports it. Graceful must not mean unbounded.
func TestShutdownForceClosesStuckCommand(t *testing.T) {
	user := newTestUser(t)
	gate := newGateSession(user) // never released
	srv := newShutdownTestServer(t, func() imapserver.Session { return gate })

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.send(t, "SELECT INBOX")
	select {
	case <-gate.selectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("SELECT never reached the backend")
	}

	r := <-shutdownAsync(srv.server, 300*time.Millisecond)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() = %v, want context.DeadlineExceeded for a stuck command", r.err)
	}
	select {
	case err := <-gate.ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("stuck backend saw ctx.Err() = %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stuck backend's context was never cancelled by the force-close")
	}
}

// TestShutdownCommandRacingBye hammers the one genuinely racy window: a client
// sends a command at the same instant Shutdown decides its connection is idle.
// Either outcome is legal -- the command completes and then BYE, or the command
// is dropped and BYE is all the client sees -- but the stream must never tear:
// exactly one BYE, always last, nothing after it, and a clean EOF rather than a
// reset. Both outcomes occur in practice; the assertion is the invariant, not
// the outcome.
func TestShutdownCommandRacingBye(t *testing.T) {
	const rounds = 40
	for i := 0; i < rounds; i++ {
		user := newTestUser(t)
		srv := newShutdownTestServer(t, func() imapserver.Session { return imapmemserver.NewUserSession(user) })

		wc := dialWire(t, srv.addr)
		wc.login(t)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			io.WriteString(wc.conn, "A9 NOOP\r\n")
		}()
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.server.Shutdown(ctx); err != nil {
				t.Errorf("round %d: Shutdown() = %v", i, err)
			}
		}()

		// Drain to EOF and inspect. A reset instead of EOF is a failure: it
		// means the server closed with the racing command unread.
		var lines []string
		wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			line, err := wc.br.ReadString('\n')
			if line != "" {
				lines = append(lines, strings.TrimRight(line, "\r\n"))
			}
			if err != nil {
				if err != io.EOF {
					t.Fatalf("round %d: read error %v; lines so far %q", i, err, lines)
				}
				break
			}
		}
		wc.conn.Close()
		wg.Wait()

		byes := 0
		for j, line := range lines {
			switch {
			case line == shutdownBye:
				byes++
				if j != len(lines)-1 {
					t.Fatalf("round %d: BYE was not the last line: %q", i, lines)
				}
			case strings.HasPrefix(line, "A9 OK"):
			default:
				t.Fatalf("round %d: unexpected line %q in %q", i, line, lines)
			}
		}
		if byes != 1 {
			t.Fatalf("round %d: saw %d BYE lines, want exactly 1: %q", i, byes, lines)
		}
	}
}
