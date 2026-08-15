package imapserver

import (
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleIdle(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	// Check if IDLE is supported by the session
	var supportsIDLE bool
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		supportsIDLE = sessionCaps.Has(imap.CapIdle) || sessionCaps.Has(imap.CapIMAP4rev2)
	} else {
		supportsIDLE = c.availableCapsSet().Has(imap.CapIdle) || c.availableCapsSet().Has(imap.CapIMAP4rev2)
	}

	if !supportsIDLE {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "IDLE not supported",
		}
	}

	if err := c.writeContReq("idling"); err != nil {
		return err
	}

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				c.server.logger().Printf("panic idling: %v\n%s", v, debug.Stack())
				done <- fmt.Errorf("imapserver: panic idling")
			}
		}()
		w := &UpdateWriter{conn: c, allowExpunge: true}
		done <- c.session.Idle(c.ctx, w, stop)
	}()

	// awaitBackend waits for the backend's Idle to return after stop is closed,
	// bounded so a backend that ignores stop cannot leak this goroutine.
	awaitBackend := func() error {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case err := <-done:
			return err
		case <-timer.C:
			c.server.logger().Printf("IDLE backend did not return within 30s after stop; goroutine leaked")
			return fmt.Errorf("imapserver: IDLE backend did not respond to stop")
		}
	}

	// Waiting for DONE is an idle point: the client is parked and nothing is in
	// flight, so Server.Shutdown may end the IDLE here rather than wait up to
	// the idle timeout for a DONE that is not coming. It does so with a plain
	// BYE and no tagged completion, which is all it has to say.
	c.setReadTimeout(idleReadTimeout)
	if !c.setIdle() {
		close(stop)
		awaitBackend()
		return errShutdown
	}
	line, isPrefix, err := c.br.ReadLine()
	active := c.setActive()
	close(stop)
	if !active {
		awaitBackend()
		return errShutdown
	}
	if err == io.EOF {
		return nil
	} else if err != nil {
		return err
	} else if isPrefix || string(line) != "DONE" {
		return newClientBugError("Syntax error: expected DONE to end IDLE command")
	}

	return awaitBackend()
}
