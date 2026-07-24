package imapclient

import (
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// Notify sends a NOTIFY command (RFC 5465).
//
// The NOTIFY command allows clients to request server-push notifications
// for mailbox events like new messages, expunges, flag changes, etc.
//
// When NOTIFY SET is active, the server may send unsolicited responses at any
// time (STATUS, FETCH, EXPUNGE, LIST responses). These unsolicited responses
// are delivered via the UnilateralDataHandler callbacks set in
// imapclient.Options.
//
// When the server sends NOTIFICATIONOVERFLOW, the NotificationOverflow callback
// in UnilateralDataHandler will be called (if set).
func (c *Client) Notify(options *imap.NotifyOptions) (*NotifyCommand, error) {
	cmd := &NotifyCommand{}
	enc := c.beginCommand("NOTIFY", cmd)
	if err := encodeNotifyOptions(enc.Encoder, options); err != nil {
		enc.end()
		return nil, err
	}
	enc.end()

	return cmd, nil
}

// encodeNotifyOptions encodes NOTIFY command options to the encoder.
func encodeNotifyOptions(enc *imapwire.Encoder, options *imap.NotifyOptions) error {
	if options == nil || len(options.Items) == 0 {
		// NOTIFY NONE: disable all notifications.
		enc.SP().Atom("NONE")
		return nil
	}

	enc.SP().Atom("SET")

	// status-indicator = SP "STATUS" (RFC 5465 section 8): a bare atom, not
	// a parenthesized list.
	if options.Status {
		enc.SP().Atom("STATUS")
	}

	for _, item := range options.Items {
		if item.MailboxSpec == "" && len(item.Mailboxes) == 0 {
			return fmt.Errorf("invalid NOTIFY item: must specify either MailboxSpec or Mailboxes")
		}

		enc.SP().Special('(')
		if item.MailboxSpec != "" {
			enc.Atom(string(item.MailboxSpec))
		} else {
			// filter-mailboxes-other = ("subtree" SP one-or-more-mailbox) /
			//                          ("mailboxes" SP one-or-more-mailbox)
			// The MAILBOXES keyword is required by the grammar; without it the
			// group is not valid RFC 5465 syntax.
			if item.Subtree {
				enc.Atom("SUBTREE")
			} else {
				enc.Atom("MAILBOXES")
			}
			enc.SP().List(len(item.Mailboxes), func(j int) {
				enc.Mailbox(item.Mailboxes[j])
			})
		}

		// events = ("(" event *(SP event) ")") / "NONE": the events part is
		// mandatory in each event-group, an empty list is spelled NONE.
		enc.SP()
		if len(item.Events) == 0 {
			enc.Atom("NONE")
		} else {
			enc.Special('(')
			for j, ev := range item.Events {
				if j > 0 {
					enc.SP()
				}
				enc.Atom(string(ev))
				if ev == imap.NotifyEventMessageNew && item.MessageNewFetch != nil {
					// MessageNew [SP "(" fetch-att *(SP fetch-att) ")"]
					enc.SP()
					writeFetchItems(enc, imapwire.NumKindSeq, item.MessageNewFetch)
				}
			}
			enc.Special(')')
		}
		enc.Special(')')
	}

	return nil
}

// NotifyCommand is a NOTIFY command.
//
// When NOTIFY SET is active, the server may send unsolicited responses at any
// time. These responses are delivered via UnilateralDataHandler
// (see Options.UnilateralDataHandler).
//
// If the server sends NOTIFICATIONOVERFLOW, the NotificationOverflow callback
// in UnilateralDataHandler will be called (if set).
type NotifyCommand struct {
	commandBase
}

// Wait blocks until the NOTIFY command has completed.
func (cmd *NotifyCommand) Wait() error {
	return cmd.wait()
}
