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
		done <- c.session.Idle(w, stop)
	}()

	c.setReadTimeout(idleReadTimeout)
	line, isPrefix, err := c.br.ReadLine()
	close(stop)
	if err == io.EOF {
		return nil
	} else if err != nil {
		return err
	} else if isPrefix || string(line) != "DONE" {
		return newClientBugError("Syntax error: expected DONE to end IDLE command")
	}

	// Wait for backend to return, with timeout to prevent goroutine leak
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		c.server.logger().Printf("IDLE backend did not return within 30s after stop; goroutine leaked")
		return fmt.Errorf("imapserver: IDLE backend did not respond to stop")
	}
}
