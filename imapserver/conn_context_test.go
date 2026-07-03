package imapserver

import (
	"context"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestConnContextCancelledByForceClose verifies the M4 plumbing: a connection's
// context is live once created and is cancelled when the server force-closes it
// (as Server.Close/Shutdown do), independently of the serve goroutine. This is
// what lets a backend blocked in a Session method be released on shutdown.
func TestConnContextCancelledByForceClose(t *testing.T) {
	srv := New(&Options{Caps: imap.CapSet{imap.CapIMAP4rev2: {}}})

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	conn := newConn(c2, srv)
	srv.mutex.Lock()
	srv.conns[conn] = struct{}{}
	srv.mutex.Unlock()

	if err := conn.Context().Err(); err != nil {
		t.Fatalf("connection context cancelled prematurely: %v", err)
	}

	srv.forceCloseConns()

	if err := conn.Context().Err(); err != context.Canceled {
		t.Fatalf("connection context after forceCloseConns = %v, want context.Canceled", err)
	}
}
