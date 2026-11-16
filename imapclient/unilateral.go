package imapclient

import (
	"fmt"
	"runtime/debug"

	"github.com/migadu/go-imap"
)

// runUnilateralHandler preserves response ordering while allowing cancellation
// to release the decoder when a handler is waiting for a command to complete.
func (c *Client) runUnilateralHandler(handler func()) error {
	if !c.resolvingSelectUpdates {
		if cmd := c.activeSelectCmd(); cmd != nil && c.State() == imap.ConnStateSelected {
			return c.deferSelectUpdate(cmd, false, func() error {
				return c.runUnilateralHandler(handler)
			}, nil)
		}
	}
	select {
	case <-c.closeCh:
		return c.cancellationErr()
	default:
	}

	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("imapclient: panic in unilateral data handler: %v\n%s", v, debug.Stack())
			}
			done <- err
		}()
		select {
		case <-c.closeCh:
			err = c.cancellationErr()
			return
		default:
		}
		handler()
	}()

	select {
	case err := <-done:
		return err
	case <-c.closeCh:
		return c.cancellationErr()
	}
}
