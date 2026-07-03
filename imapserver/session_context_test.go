package imapserver_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// ctxProbeSession embeds a real in-memory session (so every Session method and
// optional interface is implemented) but overrides Select to block until its
// context is cancelled, recording the resulting error.
type ctxProbeSession struct {
	*imapmemserver.UserSession
	selectStarted chan struct{}
	selectCtxErr  chan error
}

func (s *ctxProbeSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	close(s.selectStarted)
	<-ctx.Done()
	s.selectCtxErr <- ctx.Err()
	return nil, ctx.Err()
}

// TestSessionContextCancelledOnServerClose is an end-to-end check that the
// context handed to a blocking Session method is cancelled when the server is
// shut down while the method is in flight.
func TestSessionContextCancelledOnServerClose(t *testing.T) {
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	probe := &ctxProbeSession{
		UserSession:   imapmemserver.NewUserSession(user),
		selectStarted: make(chan struct{}),
		selectCtxErr:  make(chan error, 1),
	}

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return probe, nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go server.Serve(ln)
	defer server.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	client := imapclient.New(conn, nil)
	defer client.Close()

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login().Wait() = %v", err)
	}

	// Select blocks in the backend; run it off the test goroutine.
	go client.Select("INBOX", nil).Wait()

	select {
	case <-probe.selectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Select never reached the backend")
	}

	// The backend is blocked inside Select. Shutting the server down must cancel
	// the context it was given.
	go server.Close()

	select {
	case err := <-probe.selectCtxErr:
		if err != context.Canceled {
			t.Fatalf("backend Select context error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend context was not cancelled on server shutdown")
	}
}
