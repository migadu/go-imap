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

	cmd := &SelectCommand{
		mailbox: mailbox,
		qresync: options != nil && options.QResync != nil,
	}
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
		(state != imap.ConnStateAuthenticated || c.activeSelectCmd() == nil)
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
	if flags == nil {
		flags = []imap.Flag{}
	}

	cmd := c.activeSelectCmd()
	if cmd != nil {
		cmd.data.Flags = flags
		return c.deferSelectUpdate(cmd, true, func() error {
			return c.updateMailbox(&UnilateralDataMailbox{Flags: flags})
		}, nil)
	}
	return c.updateMailbox(&UnilateralDataMailbox{Flags: flags})
}

func (c *Client) handleExists(num uint32) error {
	if c.ignoresMailboxData() {
		return nil
	}
	cmd := c.activeSelectCmd()
	if cmd != nil {
		cmd.data.NumMessages = num
		return c.deferSelectUpdate(cmd, true, func() error {
			return c.updateMailbox(&UnilateralDataMailbox{NumMessages: &num})
		}, nil)
	}
	return c.updateMailbox(&UnilateralDataMailbox{NumMessages: &num})
}

// updateMailbox publishes one response before invoking its callback, so the
// callback observes the corresponding snapshot, including during replay.
func (c *Client) updateMailbox(data *UnilateralDataMailbox) error {
	c.mutex.Lock()
	if c.state == imap.ConnStateSelected {
		c.mailbox = c.mailbox.copy()
		if data.NumMessages != nil {
			c.mailbox.NumMessages = *data.NumMessages
		}
		if data.Flags != nil {
			c.mailbox.Flags = data.Flags
		}
		if data.PermanentFlags != nil {
			c.mailbox.PermanentFlags = data.PermanentFlags
		}
	}
	c.mutex.Unlock()
	if handler := c.options.unilateralDataHandler().Mailbox; handler != nil {
		return c.runUnilateralHandler(func() { handler(data) })
	}
	return nil
}

// Until CLOSED or tagged completion, responses to a re-selection can still
// belong to the previous selection. Keep their callbacks in wire order. The
// reader alone owns this journal; callbacks run without Client.mutex held.
type selectUpdate struct {
	apply       func() error
	discard     func()
	provisional bool
	size        int64
}

func (c *Client) deferSelectUpdate(cmd *SelectCommand, provisional bool, apply func() error, discard func()) error {
	syntax, literals := c.dec.ReadCount()
	return c.deferSelectUpdateSize(cmd, provisional, apply, discard, syntax+literals+64)
}

func (c *Client) deferSelectUpdateSize(cmd *SelectCommand, provisional bool, apply func() error, discard func(), size int64) error {
	if cmd == nil || c.State() != imap.ConnStateSelected {
		// An opening selection owns provisional data outright. Definite
		// unilateral responses are still delivered normally.
		if provisional {
			if discard != nil {
				discard()
			}
			return nil
		}
		return apply()
	}
	// A fixed overhead bounds even empty responses and their journal entries.
	if err := c.tryAcquireFetchBuffer(size); err != nil {
		if discard != nil {
			discard()
		}
		return err
	}
	cmd.updates = append(cmd.updates, selectUpdate{apply, discard, provisional, size})
	return nil
}

func (c *Client) resolveSelectUpdates(cmd *SelectCommand, replayProvisional bool) (err error) {
	updates := cmd.updates
	cmd.updates = nil
	c.resolvingSelectUpdates = true
	defer func() { c.resolvingSelectUpdates = false }()
	for _, update := range updates {
		if err == nil && (replayProvisional || !update.provisional) {
			err = update.apply()
		} else if update.discard != nil {
			update.discard()
		}
		c.releaseFetchBuffer(update.size)
	}
	return err
}

func (c *Client) discardSelectUpdates(cmd *SelectCommand) {
	for _, update := range cmd.updates {
		if update.discard != nil {
			update.discard()
		}
		c.releaseFetchBuffer(update.size)
	}
	cmd.updates = nil
}

// SelectCommand is a SELECT command.
type SelectCommand struct {
	commandBase
	mailbox  string
	qresync  bool
	recorded bool
	data     imap.SelectData
	updates  []selectUpdate
}

func (cmd *SelectCommand) Wait() (*imap.SelectData, error) {
	return &cmd.data, cmd.wait()
}

type unselectCommand struct {
	Command
}
