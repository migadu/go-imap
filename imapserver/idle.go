package imapserver

import (
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// interruptRead unblocks a read in progress on another goroutine by arming a
// read deadline that has already passed. The caller is responsible for putting
// a sane deadline back (setReadTimeout) once the read has returned.
//
// c.conn is read under the mutex, unlike setReadTimeout's: this is called from
// a watcher goroutine rather than the command loop, and STARTTLS reassigns the
// field.
func (c *Conn) interruptRead() {
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()
	conn.SetReadDeadline(time.Now().Add(-time.Second))
}

// isTimeoutError reports whether err is an expired deadline. It is deliberately
// separate from isConnectionClosedError: a timeout means the peer is silent,
// not gone, so there is still a socket to say BYE on.
func isTimeoutError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

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
	// idleErr is published by closing ended: the assignment happens before the
	// close on both the normal and the panic path (defers run LIFO, so the
	// close is registered first and runs last), and every reader below reaches
	// it through a receive on ended, so the handoff needs no lock.
	var idleErr error
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		defer func() {
			if v := recover(); v != nil {
				c.server.logger().Printf("panic idling: %v\n%s", v, debug.Stack())
				idleErr = fmt.Errorf("imapserver: panic idling")
			}
		}()
		w := &UpdateWriter{conn: c, allowExpunge: true}
		idleErr = c.session.Idle(c.ctx, w, stop)
	}()

	// awaitBackend waits for the backend's Idle to return after stop is closed,
	// bounded so a backend that ignores stop cannot leak this goroutine.
	awaitBackend := func() error {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ended:
			return idleErr
		case <-timer.C:
			c.server.logger().Printf("IDLE backend did not return within 30s after stop; goroutine leaked")
			return fmt.Errorf("imapserver: IDLE backend did not respond to stop")
		}
	}

	// A backend can end an IDLE it no longer has anything to idle on — the
	// selected mailbox stopped being serveable (UIDVALIDITY changed, the
	// mailbox was deleted), so it returns a BYE-typed error. The client has to
	// hear that NOW: the read below blocks until DONE, which RFC 2177 puts up
	// to 29 minutes away, so without this the backend's decision was not even
	// LOOKED at until then — and a backend that wanted to disconnect sooner had
	// to reach around the library and close the socket itself.
	//
	// The read is blocking, so it is interrupted with a read deadline in the
	// past; backendEnded is what tells the resulting timeout apart from the
	// client genuinely going quiet.
	//
	// The idle deadline is armed HERE, before the watcher exists: the interrupt
	// is itself a deadline, so an interrupt that landed before the arm would be
	// overwritten by it and the read below would block on the 35-minute window
	// as if the backend had said nothing. A backend that fails the moment it is
	// asked to idle (the mailbox was already gone when IDLE arrived) is exactly
	// the one that races this. Ordering against setIdle is unchanged: the arm
	// still precedes it, so requestShutdown's own past deadline still wins.
	//
	// Only a BYE-typed error is acted on. A backend returning nil has merely
	// stopped idling — the client is still parked and still owes a DONE, and
	// ending the command under it would leave the two sides disagreeing about
	// whether the IDLE is over. Any OTHER error (a write failure, a panic) is
	// held for the DONE exactly as before: it becomes the tagged NO that
	// answers the DONE, which is a completion the client is waiting for,
	// whereas answering it early hands the client a tagged line while it still
	// owes a DONE — and a BYE is the only completion that has no such
	// follow-up, because it ends the connection.
	c.setReadTimeout(idleReadTimeout)
	var backendEnded atomic.Bool
	watchStop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ended:
			if isByeError(idleErr) {
				backendEnded.Store(true)
				c.interruptRead()
			}
		case <-watchStop:
		}
	}()
	defer func() {
		// Join the watcher BEFORE restoring the deadline, so an interrupt can
		// never land after the restore and leave a deadline in the past armed
		// for readCommand's DiscardLine (which would read as a desynchronized
		// command stream and end the connection with the wrong reason).
		close(watchStop)
		<-watcherDone
		c.setReadTimeout(cmdReadTimeout)
	}()

	// Waiting for DONE is an idle point: the client is parked and nothing is in
	// flight, so Server.Shutdown may end the IDLE here rather than wait up to
	// the idle timeout for a DONE that is not coming. It does so with a plain
	// BYE and no tagged completion, which is all it has to say.
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
	if backendEnded.Load() {
		// The backend ended the IDLE with a BYE, so that is the answer and err
		// (if any) is this handler's own interrupt rather than anything the
		// client did. Checked before err on purpose: a DONE arriving in the same
		// instant is not a reason to keep serving a mailbox the backend just
		// said it cannot serve — the BYE answers that DONE correctly too, since
		// it ends the connection either way.
		return awaitBackend()
	}
	if err != nil {
		switch {
		case isConnectionClosedError(err):
			// The client hung up mid-IDLE (EOF, RST, a proxy tearing the
			// connection down). Ordinary, and there is nobody left to tell.
			return nil
		case isTimeoutError(err):
			// The read deadline expired: the client stopped re-issuing IDLE, or
			// something outside the library (a connection guard's inactivity
			// deadline) armed a shorter one. Either way it is an INACTIVITY
			// TIMEOUT, and RFC 9051 §5.4 says to announce that with an untagged
			// BYE before closing.
			//
			// Returning the raw error instead answered the client's IDLE with
			// `NO [SERVERBUG] Internal server error` — telling it a routine
			// timeout was a server malfunction, on a connection the serve loop
			// then went on reading — and logged every such disconnect as a
			// command failure.
			return &imap.Error{
				Type: imap.StatusResponseTypeBye,
				Text: "Idle timeout",
			}
		default:
			return err
		}
	}
	if isPrefix || string(line) != "DONE" {
		return newClientBugError("Syntax error: expected DONE to end IDLE command")
	}

	return awaitBackend()
}
