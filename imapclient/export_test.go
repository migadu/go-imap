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

// SaveSearchForTest establishes a non-empty saved result in a scripted
// session before testing responses that refer to the SEARCHRES marker.
func (c *Client) SaveSearchForTest(uid bool) error {
	name := "SEARCH"
	if uid {
		name = "UID SEARCH"
	}
	cmd := &SearchCommand{}
	enc := c.beginCommand(name, cmd)
	enc.SP().Atom("RETURN").SP().Special('(').Atom("SAVE").Special(')').SP().Atom("ALL")
	enc.end()
	_, err := cmd.Wait()
	return err
}
