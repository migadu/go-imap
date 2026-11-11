package imapclient

import (
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
)

// Select sends a SELECT or EXAMINE command.
//
// A nil options pointer is equivalent to a zero options value.
func (c *Client) Select(mailbox string, options *imap.SelectOptions) *SelectCommand {
	cmdName := "SELECT"
	if options != nil && options.ReadOnly {
		cmdName = "EXAMINE"
	}

	cmd := &SelectCommand{mailbox: mailbox}
	enc := c.beginCommand(cmdName, cmd)
	enc.SP().Mailbox(mailbox)

	if options != nil {
		if options.CondStore {
			enc.SP().Special('(').Atom("CONDSTORE").Special(')')
		} else if options.QResync != nil {
			enc.SP().Special('(').Atom("QRESYNC").SP().Special('(')
			enc.Number(options.QResync.UIDValidity)
			enc.SP().ModSeq(options.QResync.ModSeq)
			if len(options.QResync.KnownUIDs) > 0 {
				enc.SP().NumSet(options.QResync.KnownUIDs)
				if options.QResync.SeqMatch != nil {
					enc.SP().Special('(')
					enc.NumSet(options.QResync.SeqMatch.SeqNums)
					enc.SP().NumSet(options.QResync.SeqMatch.UIDs)
					enc.Special(')')
				}
			}
			enc.Special(')').Special(')')
		}
	}

	enc.end()
	return cmd
}

// Unselect sends an UNSELECT command.
//
// This command requires support for IMAP4rev2 or the UNSELECT extension.
func (c *Client) Unselect() *Command {
	cmd := &unselectCommand{}
	c.beginCommand("UNSELECT", cmd).end()
	return &cmd.Command
}

// UnselectAndExpunge sends a CLOSE command.
//
// CLOSE implicitly performs a silent EXPUNGE command.
func (c *Client) UnselectAndExpunge() *Command {
	cmd := &unselectCommand{}
	c.beginCommand("CLOSE", cmd).end()
	return &cmd.Command
}

// ignoresMailboxData reports whether a response about the selected mailbox
// or its messages is to be read and dropped. RFC 9051 section 11.3: "when
// not in selected state, clients MUST ignore responses / response codes
// related to message and mailbox status such as FLAGS, EXIST, EXPUNGE, and
// FETCH". That holds for the responses to a pending FETCH, STORE or EXPUNGE
// too. The responses to SELECT and EXAMINE arrive before the client enters
// that state, and are not ignored.
func (c *Client) ignoresMailboxData() bool {
	c.mutex.Lock()
	state := c.state
	c.mutex.Unlock()
	// Only an authenticated connection can legitimately be selecting a
	// mailbox. A queued SELECT before authentication, or after cancellation,
	// must not make otherwise unsuitable responses meaningful.
	return state != imap.ConnStateSelected &&
		(state != imap.ConnStateAuthenticated || findPendingCmdByType[*SelectCommand](c) == nil)
}

// ignoresUnsolicitedData reports whether a response no command asked for is
// to be dropped. RFC 9051 section 11.3: "Before authentication, clients
// SHOULD ignore any responses other than CAPABILITY and server status
// responses".
func (c *Client) ignoresUnsolicitedData() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.state != imap.ConnStateAuthenticated && c.state != imap.ConnStateSelected
}

func (c *Client) handleFlags() error {
	flags, err := internal.ExpectFlagList(c.dec)
	if err != nil {
		return err
	}
	if c.ignoresMailboxData() {
		return nil
	}

	c.mutex.Lock()
	if c.state == imap.ConnStateSelected {
		c.mailbox = c.mailbox.copy()
		c.mailbox.Flags = flags
	}
	c.mutex.Unlock()

	cmd := findPendingCmdByType[*SelectCommand](c)
	if cmd != nil {
		cmd.data.Flags = flags
	} else if handler := c.options.unilateralDataHandler().Mailbox; handler != nil {
		return c.runUnilateralHandler(func() { handler(&UnilateralDataMailbox{Flags: flags}) })
	}

	return nil
}

func (c *Client) handleExists(num uint32) error {
	if c.ignoresMailboxData() {
		return nil
	}
	cmd := findPendingCmdByType[*SelectCommand](c)
	if cmd != nil {
		cmd.data.NumMessages = num
	} else {
		c.mutex.Lock()
		if c.state == imap.ConnStateSelected {
			c.mailbox = c.mailbox.copy()
			c.mailbox.NumMessages = num
		}
		c.mutex.Unlock()

		if handler := c.options.unilateralDataHandler().Mailbox; handler != nil {
			return c.runUnilateralHandler(func() { handler(&UnilateralDataMailbox{NumMessages: &num}) })
		}
	}
	return nil
}

// SelectCommand is a SELECT command.
type SelectCommand struct {
	commandBase
	mailbox string
	data    imap.SelectData
}

func (cmd *SelectCommand) Wait() (*imap.SelectData, error) {
	return &cmd.data, cmd.wait()
}

type unselectCommand struct {
	Command
}
