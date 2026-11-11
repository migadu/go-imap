package imapclient

import "github.com/migadu/go-imap"

// SetStateForTest waits for the greeting and then puts the client in the
// given connection state, without the commands that lead there. Tests which
// script only the responses under test use it to stand for a session that has
// authenticated, or selected a mailbox, before the test begins.
func (c *Client) SetStateForTest(state imap.ConnState) {
	if err := c.WaitGreeting(); err != nil {
		return
	}
	c.mutex.Lock()
	c.state = state
	if state == imap.ConnStateSelected && c.mailbox == nil {
		c.mailbox = &SelectedMailbox{Name: "INBOX"}
	}
	c.mutex.Unlock()
}
