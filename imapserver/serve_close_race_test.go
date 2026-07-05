package imapserver

import (
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestServeCloseNoWaitGroupRace exercises the Serve/Close handoff: Close (and
// Shutdown) call listenerWaitGroup.Wait after marking the server closed, so
// Serve must register with the wait group under the same mutex, and Done must
// run only after Serve's cleanup. An Add that slips in after Close's locked
// section is a 0->1 counter transition concurrent with Wait — a
// sync.WaitGroup misuse — and a Done that fires before the listeners-map
// delete lets Close return while Serve is still cleaning up. The historical
// code had both flaws; the leftover-listener assertion below fails
// immediately against it.
func TestServeCloseNoWaitGroupRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		srv := New(&Options{Caps: imap.CapSet{imap.CapIMAP4rev2: {}}})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ln) }()
		// Wait until Serve has registered the listener, then Close right
		// away: this lands Close's Wait inside the historical window between
		// Serve releasing the mutex and calling Add.
		for {
			srv.mutex.Lock()
			registered := len(srv.listeners) == 1
			srv.mutex.Unlock()
			if registered {
				break
			}
			runtime.Gosched()
		}
		if err := srv.Close(); err != nil && err != errClosed {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		// Close must not return until Serve has fully unregistered: Done is
		// ordered after the listeners-map delete, so a leftover entry here
		// means Wait did not cover Serve.
		srv.mutex.Lock()
		leftover := len(srv.listeners)
		srv.mutex.Unlock()
		if leftover != 0 {
			t.Fatalf("iteration %d: Close returned with %d listener(s) still registered", i, leftover)
		}
		if err := <-done; err != nil && err != errClosed {
			t.Fatalf("iteration %d: Serve: %v", i, err)
		}
		ln.Close()
	}
}

// TestConnServeAfterClose covers the other half of the Close handoff: a
// connection accepted just before Close whose serve goroutine has not yet
// registered in s.conns. Close sets closed and then forceCloseConns snapshots
// the map under the same mutex, so a registration that happens after the
// snapshot would never be force-closed and the session would run to
// completion on a closed server. serve must therefore check closed at
// registration time and bail out with a best-effort BYE, without creating a
// session.
func TestConnServeAfterClose(t *testing.T) {
	var sessionStarted atomic.Bool
	srv := New(&Options{
		NewSession: func(c *Conn) (Session, *GreetingData, error) {
			sessionStarted.Store(true)
			return nil, nil, &imap.Error{Type: imap.StatusResponseTypeBye, Text: "rejected"}
		},
		Caps: imap.CapSet{imap.CapIMAP4rev2: {}},
	})
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		newConn(serverConn, srv).serve()
		close(done)
	}()

	// The client must see a BYE and then the connection drop — no greeting,
	// no session.
	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("reading BYE: %v", err)
	}
	if got := string(buf[:n]); !strings.HasPrefix(got, "* BYE") {
		t.Fatalf("connection served after Close: read %q, want a * BYE line", got)
	}
	if n, err := clientConn.Read(buf); n != 0 || err == nil {
		t.Fatalf("connection still open after BYE: read %d bytes (%q), err = %v", n, buf[:n], err)
	}
	<-done

	if sessionStarted.Load() {
		t.Fatal("NewSession was called on a closed server")
	}
	srv.mutex.Lock()
	leftover := len(srv.conns)
	srv.mutex.Unlock()
	if leftover != 0 {
		t.Fatalf("%d connection(s) left registered on a closed server", leftover)
	}
}
