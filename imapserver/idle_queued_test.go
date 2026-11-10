package imapserver_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// newQueuedIdleServer runs an in-memory server advertising IDLE, with one user
// and an INBOX.
func newQueuedIdleServer(t *testing.T) string {
	t.Helper()

	user := newTestUser(t)
	mem := imapmemserver.New()
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}},
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	return ln.Addr().String()
}

func dialQueuedIdleClient(t *testing.T, addr string, opts *imapclient.Options) *imapclient.Client {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c := imapclient.New(conn, opts)
	t.Cleanup(func() { c.Close() })
	if err := c.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	return c
}

// deliver appends a message over a separate connection, the way another
// session (an LMTP delivery, a second client) would.
func deliverQueuedIdleMessage(t *testing.T, addr string) {
	t.Helper()

	c := dialQueuedIdleClient(t, addr, nil)
	body := strings.Join([]string{
		"From: <sender@example.com>",
		"To: <user@example.com>",
		"Subject: delivered while nobody was idling",
		"",
		"body",
		"",
	}, "\r\n")

	cmd := c.Append("INBOX", int64(len(body)), nil)
	if _, err := cmd.Write([]byte(body)); err != nil {
		t.Fatalf("writing APPEND literal: %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("closing APPEND literal: %v", err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("APPEND: %v", err)
	}
}

// An update queued while nothing is listening must still be announced by the
// next IDLE.
//
// SessionTracker.queueUpdate always appends to the queue, but signals a
// listener with a non-blocking send, and t.updates is nil whenever Idle is not
// running -- so an update queued outside an IDLE leaves no signal behind. Idle
// polls only when signalled, so without the drain at registration it would sit
// on the queued update and wait for the NEXT one to wake it, which on a quiet
// mailbox may never arrive.
//
// The same hole is reachable at startup, since handleIdle writes the
// "+ idling" continuation before the goroutine that calls Idle gets to
// register: the client is told it is idling before anything is listening. That
// form is a race; this one -- a delivery between DONE and the next IDLE -- is
// deterministic and has the same cause.
//
// Upstream report: emersion/go-imap#767 by DP-Brian.
func TestIdleAnnouncesUpdatesQueuedWhileNotIdling(t *testing.T) {
	addr := newQueuedIdleServer(t)

	exists := make(chan uint32, 8)
	c := dialQueuedIdleClient(t, addr, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data != nil && data.NumMessages != nil {
					select {
					case exists <- *data.NumMessages:
					default:
					}
				}
			},
		},
	})

	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	// Idle and stop again, the way a client does when it needs the connection
	// to run a command. The session tracker now has no listener.
	idle, err := c.Idle()
	if err != nil {
		t.Fatalf("IDLE: %v", err)
	}
	if err := idle.Close(); err != nil {
		t.Fatalf("DONE: %v", err)
	}
	for len(exists) > 0 {
		<-exists
	}

	// Delivered by somebody else while this client is not idling.
	deliverQueuedIdleMessage(t, addr)

	idle, err = c.Idle()
	if err != nil {
		t.Fatalf("second IDLE: %v", err)
	}
	defer idle.Close()

	select {
	case n := <-exists:
		if n == 0 {
			t.Errorf("EXISTS announced %d messages", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the message delivered between DONE and IDLE was never announced")
	}
}
