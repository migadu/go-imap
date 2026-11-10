package imapclient

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/migadu/go-imap"
)

const idleRestartInterval = 28 * time.Minute

// Idle sends an IDLE command.
//
// Unlike other commands, this method blocks until the server acknowledges it.
// On success, the IDLE command is running and other commands cannot be sent.
// The caller must invoke IdleCommand.Close to stop IDLE and unblock the
// client.
//
// This command requires support for IMAP4rev2 or the IDLE extension. The IDLE
// command is restarted automatically to avoid getting disconnected due to
// inactivity timeouts.
func (c *Client) Idle() (*IdleCommand, error) {
	return c.startIdle(false)
}

// startIdle is Idle. auto is set for the IDLE the client runs on its own
// account (Options.AutoIdle), which is not itself a reason to end one.
func (c *Client) startIdle(auto bool) (*IdleCommand, error) {
	child, err := c.idle(auto)
	if err != nil {
		return nil, err
	}

	cmd := &IdleCommand{
		auto: auto,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go cmd.run(c, child)
	return cmd, nil
}

// armAutoIdle starts the wait after which the client idles on its own, if
// that is asked for and no command is in flight. c.mutex must be held.
func (c *Client) armAutoIdleLocked() {
	if c.options.AutoIdle <= 0 || c.closed || len(c.pendingCmds) > 0 {
		return
	}
	if c.autoIdleTimer == nil {
		c.autoIdleTimer = time.AfterFunc(c.options.AutoIdle, c.startAutoIdle)
	} else {
		c.autoIdleTimer.Reset(c.options.AutoIdle)
	}
}

// startAutoIdle runs when the client has had no command in flight for
// Options.AutoIdle.
func (c *Client) startAutoIdle() {
	c.autoIdleMutex.Lock()
	defer c.autoIdleMutex.Unlock()

	c.mutex.Lock()
	// IDLE is for the authenticated and selected states, and for servers
	// that offer it (RFC 2177; part of IMAP4rev2). With the capabilities
	// unknown for now, the command which fetches them arms the timer anew.
	authenticated := c.state == imap.ConnStateAuthenticated || c.state == imap.ConnStateSelected
	ok := !c.closed && !c.autoIdleRefused && c.autoIdle == nil && len(c.pendingCmds) == 0 &&
		authenticated && (c.caps.Has(imap.CapIdle) || c.caps.Has(imap.CapIMAP4rev2))
	c.mutex.Unlock()
	if !ok {
		return
	}

	cmd, err := c.startIdle(true)
	if err != nil {
		// A server that offers IDLE and refuses it will refuse it again.
		var imapErr *imap.Error
		if errors.As(err, &imapErr) {
			c.mutex.Lock()
			c.autoIdleRefused = true
			c.mutex.Unlock()
		}
		return
	}
	c.mutex.Lock()
	c.autoIdle = cmd
	c.mutex.Unlock()
}

// stopAutoIdleLocked ends the IDLE the client started on its own, if there
// is one, so that a command can be sent. It returns once DONE is written; RFC
// 2177 section 3 lets the next command follow it at once.
//
// c.autoIdleMutex must be held.
func (c *Client) stopAutoIdleLocked() {
	c.mutex.Lock()
	if c.autoIdleTimer != nil {
		c.autoIdleTimer.Stop()
	}
	cmd := c.autoIdle
	c.autoIdle = nil
	c.mutex.Unlock()

	if cmd != nil {
		cmd.Close()
	}
}

// IdleCommand is an IDLE command.
//
// Initially, the IDLE command is running. The server may send unilateral
// data. The client cannot send any command while IDLE is running.
//
// Close must be called to stop the IDLE command.
type IdleCommand struct {
	auto    bool
	stopped atomic.Bool
	stop    chan struct{}
	done    chan struct{}

	err       error
	lastChild *idleCommand
}

func (cmd *IdleCommand) run(c *Client, child *idleCommand) {
	defer close(cmd.done)

	timer := time.NewTimer(idleRestartInterval)
	defer timer.Stop()

	defer func() {
		if child != nil {
			if err := child.Close(); err != nil && cmd.err == nil {
				cmd.err = err
			}
		}
	}()

	for {
		select {
		case <-timer.C:
			timer.Reset(idleRestartInterval)

			if cmd.err = child.Close(); cmd.err != nil {
				return
			}
			if child, cmd.err = c.idle(cmd.auto); cmd.err != nil {
				return
			}
		case <-c.decCh:
			cmd.lastChild = child
			return
		case <-cmd.stop:
			cmd.lastChild = child
			return
		}
	}
}

// Close stops the IDLE command.
//
// This method blocks until the command to stop IDLE is written, but doesn't
// wait for the server to respond. Callers can use Wait for this purpose.
func (cmd *IdleCommand) Close() error {
	if cmd.stopped.Swap(true) {
		return fmt.Errorf("imapclient: IDLE already closed")
	}
	close(cmd.stop)
	<-cmd.done
	return cmd.err
}

// Wait blocks until the IDLE command has completed.
func (cmd *IdleCommand) Wait() error {
	<-cmd.done
	if cmd.err != nil {
		return cmd.err
	}
	if cmd.lastChild == nil {
		// Defensive: every err==nil exit path sets lastChild, but guard against a
		// future path that doesn't rather than nil-dereference here.
		return cmd.err
	}
	return cmd.lastChild.Wait()
}

func (c *Client) idle(auto bool) (*idleCommand, error) {
	cmd := &idleCommand{auto: auto}
	contReq := c.registerContReq(cmd)
	cmd.enc = c.beginCommand("IDLE", cmd)
	cmd.enc.flush()

	_, err := contReq.Wait()
	if err != nil {
		cmd.enc.end()
		return nil, err
	}

	return cmd, nil
}

// idleCommand represents a singular IDLE command, without the restart logic.
type idleCommand struct {
	commandBase
	enc *commandEncoder
	// auto is set for an IDLE the client runs on its own account.
	auto bool
}

// Close stops the IDLE command.
//
// This method blocks until the command to stop IDLE is written, but doesn't
// wait for the server to respond. Callers can use Wait for this purpose.
func (cmd *idleCommand) Close() error {
	if cmd.err != nil {
		return cmd.err
	}
	if cmd.enc == nil {
		return fmt.Errorf("imapclient: IDLE command closed twice")
	}
	cmd.enc.client.setWriteTimeout(cmdWriteTimeout)
	_, err := cmd.enc.client.bw.WriteString("DONE\r\n")
	if err == nil {
		err = cmd.enc.client.bw.Flush()
	}
	cmd.enc.end()
	cmd.enc = nil
	return err
}

// Wait blocks until the IDLE command has completed.
//
// Wait can only be called after Close.
func (cmd *idleCommand) Wait() error {
	if cmd.enc != nil {
		panic("imapclient: idleCommand.Close must be called before Wait")
	}
	return cmd.wait()
}
