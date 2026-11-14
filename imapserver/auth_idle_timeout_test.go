package imapserver_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func newAuthIdleTimeoutServer(t *testing.T, timeout time.Duration) string {
	t.Helper()
	user := newTestUser(t)
	mem := imapmemserver.New()
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth:    true,
		Caps:            imap.CapSet{imap.CapIMAP4rev1: {}},
		AuthIdleTimeout: timeout,
	})
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return ln.Addr().String()
}

// The server closes a connection that has not authenticated once it has been
// idle for AuthIdleTimeout, not after the fixed 30 seconds it used before.
func TestAuthIdleTimeoutClosesUnauthenticatedConnection(t *testing.T) {
	wc := dialWire(t, newAuthIdleTimeoutServer(t, 300*time.Millisecond))

	start := time.Now()
	wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := wc.br.ReadString('\n')
	if !errors.Is(err, io.EOF) {
		t.Fatalf("idle unauthenticated connection: read %q, %v; want the server to close it", line, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("closed after %v, want about the 300ms AuthIdleTimeout", elapsed)
	}
}

// Every command restarts the wait, and after authentication the timeout no
// longer applies.
func TestAuthIdleTimeoutOnlyBetweenCommandsBeforeAuthentication(t *testing.T) {
	wc := dialWire(t, newAuthIdleTimeoutServer(t, 300*time.Millisecond))

	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		wc.mustOK(t, "NOOP")
	}
	wc.login(t)
	time.Sleep(600 * time.Millisecond)
	wc.mustOK(t, "NOOP")
}
