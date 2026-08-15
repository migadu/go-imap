package imapserver_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// notifyProbeSession embeds a real in-memory session and replaces the NOTIFY
// pump with one that only records its own lifecycle: it never delivers events,
// it just reports when it was started and when it observed stop.
type notifyProbeSession struct {
	*imapmemserver.UserSession
	pumpStarted chan struct{}
	pumpStopped chan struct{}
}

func (s *notifyProbeSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	close(s.pumpStarted)
	<-stop
	close(s.pumpStopped)
	return nil
}

// TestShutdownStopsNotifyPumpBeforeBye: BYE announces that the server is done
// talking (RFC 9051 §7.1.5), so nothing may still be producing output when it
// goes out. The NOTIFY pump writes unsolicited responses from its own
// goroutine, independent of the command loop, and it must therefore be stopped
// before the shutdown BYE is written -- not in the deferred teardown that runs
// only after the lingering close.
//
// The observable property is ordering: at the instant the client reads the
// BYE, the pump must already have observed stop. With the pump stopped only in
// teardown it is still running at that instant, and it stops only after the
// client closes its side, which the client here does not do until it has made
// the check.
func TestShutdownStopsNotifyPumpBeforeBye(t *testing.T) {
	user := newTestUser(t)
	probe := &notifyProbeSession{
		UserSession: imapmemserver.NewUserSession(user),
		pumpStarted: make(chan struct{}),
		pumpStopped: make(chan struct{}),
	}
	srv := newShutdownTestServer(t, func() imapserver.Session { return probe }, imap.CapNotify)

	wc := dialWire(t, srv.addr)
	wc.login(t)
	wc.mustOK(t, "SELECT INBOX")
	wc.mustOK(t, "NOTIFY SET (SELECTED (MessageNew MessageExpunge))")
	select {
	case <-probe.pumpStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("NOTIFY pump never started")
	}

	clientErr := make(chan error, 1)
	go func() {
		line, err := wc.readLine()
		if err != nil {
			clientErr <- err
			return
		}
		if line != shutdownBye {
			clientErr <- fmt.Errorf("after shutdown got %q, want %q", line, shutdownBye)
			return
		}
		// The check itself: the BYE is on the wire, so the pump must be gone.
		select {
		case <-probe.pumpStopped:
		default:
			clientErr <- fmt.Errorf("NOTIFY pump was still running when the client received BYE")
			return
		}
		wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := wc.br.ReadByte(); err != io.EOF {
			clientErr <- fmt.Errorf("after BYE got %v, want a clean EOF", err)
			return
		}
		clientErr <- wc.conn.Close()
	}()

	checkPrompt(t, <-shutdownAsync(srv.server, 5*time.Second), "a NOTIFY-watching but idle connection was open")
	if err := <-clientErr; err != nil {
		t.Error(err)
	}
}
