package imapserver_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func newAuthIdleTimeoutServerInstance(t *testing.T, timeout time.Duration) (*imapserver.Server, string) {
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
	return server, ln.Addr().String()
}

func newAuthIdleTimeoutServer(t *testing.T, timeout time.Duration) string {
	t.Helper()
	_, addr := newAuthIdleTimeoutServerInstance(t, timeout)
	return addr
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

// An empty line ("\r\n") returns the connection to the between-commands idle
// state rather than parking inside readCommand under cmdReadTimeout (30s) with
// c.idle == false: AuthIdleTimeout must still govern unauthenticated idle
// waits and Server.Shutdown must still wake the connection with * BYE.
func TestAuthIdleTimeoutAndShutdownAfterEmptyLine(t *testing.T) {
	t.Run("AuthIdleTimeoutStillApplies", func(t *testing.T) {
		wc := dialWire(t, newAuthIdleTimeoutServer(t, 250*time.Millisecond))

		if _, err := io.WriteString(wc.conn, "\r\n"); err != nil {
			t.Fatalf("WriteString(CRLF) = %v", err)
		}

		start := time.Now()
		wc.conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		line, err := wc.br.ReadString('\n')
		if !errors.Is(err, io.EOF) {
			t.Fatalf("after empty line, unauthenticated connection stayed open past 1.5s: read %q, %v", line, err)
		}
		if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
			t.Errorf("closed after %v, want ~250ms AuthIdleTimeout", elapsed)
		}
	})

	t.Run("ShutdownWakesConnectionWithBye", func(t *testing.T) {
		srv, addr := newAuthIdleTimeoutServerInstance(t, 5*time.Second)
		wc := dialWire(t, addr)

		if _, err := io.WriteString(wc.conn, "\r\n"); err != nil {
			t.Fatalf("WriteString(CRLF) = %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			done <- srv.Shutdown(ctx)
		}()

		wc.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		bye, err := wc.br.ReadString('\n')
		if err != nil || !strings.HasPrefix(bye, "* BYE ") {
			t.Fatalf("Shutdown() did not wake connection with * BYE after empty line: got %q, %v", bye, err)
		}
		wc.conn.Close()

		if err := <-done; err != nil {
			t.Fatalf("Shutdown() stalled on connection after empty line: %v", err)
		}
	})
}
