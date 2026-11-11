package imapclient

import (
	"fmt"
	"runtime/debug"
)

// runUnilateralHandler preserves response ordering while allowing cancellation
// to release the decoder when a handler is waiting for a command to complete.
func (c *Client) runUnilateralHandler(handler func()) error {
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
