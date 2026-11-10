package imapclient

import (
	"io"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
)

// Append sends an APPEND command.
//
// The caller must call AppendCommand.Close.
//
// The options are optional.
func (c *Client) Append(mailbox string, size int64, options *imap.AppendOptions) *AppendCommand {
	cmd := &AppendCommand{}
	cmd.enc = c.beginCommand("APPEND", cmd)
	writeAppendArgs(cmd.enc, mailbox, options)
	// TODO: literal8 for BINARY
	// TODO: UTF8 data ext for UTF8=ACCEPT, with literal8
	cmd.wc = cmd.enc.Literal(size)
	return cmd
}

// AppendCommand is an APPEND command.
//
// Callers must write the message contents, then call Close.
type AppendCommand struct {
	commandBase
	enc  *commandEncoder
	wc   io.WriteCloser
	data imap.AppendData
}

func (cmd *AppendCommand) Write(b []byte) (int, error) {
	return cmd.wc.Write(b)
}

func (cmd *AppendCommand) Close() error {
	err := cmd.wc.Close()
	if cmd.enc != nil {
		cmd.enc.end()
		cmd.enc = nil
	}
	return err
}

func (cmd *AppendCommand) Wait() (*imap.AppendData, error) {
	return &cmd.data, cmd.wait()
}

// writeAppendArgs writes the arguments of APPEND up to the message data.
func writeAppendArgs(enc *commandEncoder, mailbox string, options *imap.AppendOptions) {
	enc.SP().Mailbox(mailbox).SP()
	if options != nil && len(options.Flags) > 0 {
		enc.List(len(options.Flags), func(i int) {
			enc.Flag(options.Flags[i])
		}).SP()
	}
	if options != nil && !options.Time.IsZero() {
		enc.String(options.Time.Format(internal.DateTimeLayout)).SP()
	}
}

// AppendCatenate sends an APPEND command which has the server assemble the
// message from parts: literals, and URLs of messages or message parts it
// already holds.
//
// The caller adds at least one part with AppendCatenateCommand.URL and
// AppendCatenateCommand.Text, in the order they go in the message, then calls
// AppendCatenateCommand.Close.
//
// The options are optional.
//
// This command requires support for the CATENATE extension (RFC 4469).
func (c *Client) AppendCatenate(mailbox string, options *imap.AppendOptions) *AppendCatenateCommand {
	cmd := &AppendCatenateCommand{}
	cmd.enc = c.beginCommand("APPEND", cmd)
	writeAppendArgs(cmd.enc, mailbox, options)
	cmd.enc.Atom("CATENATE").SP().Special('(')
	return cmd
}

// AppendCatenateCommand is an APPEND command with the CATENATE extension.
//
// Callers must add the parts of the message, then call Close.
type AppendCatenateCommand struct {
	commandBase
	enc      *commandEncoder
	numParts int
	data     imap.AppendData
}

func (cmd *AppendCatenateCommand) part() *commandEncoder {
	if cmd.numParts > 0 {
		cmd.enc.SP()
	}
	cmd.numParts++
	return cmd.enc
}

// URL adds the octets an IMAP URL (RFC 5092) points to. RFC 4469 section 3
// only provides for a relative URL of a message or message part on this
// server, such as "/Drafts;UIDVALIDITY=385759045/;UID=20/;section=1.MIME".
func (cmd *AppendCatenateCommand) URL(url string) {
	cmd.part().Atom("URL").SP().String(url)
}

// Text adds a literal of size octets. The caller must write exactly that
// many to the returned writer and close it before adding another part.
func (cmd *AppendCatenateCommand) Text(size int64) io.WriteCloser {
	cmd.part().Atom("TEXT").SP()
	return cmd.enc.Literal(size)
}

// Close ends the list of parts.
func (cmd *AppendCatenateCommand) Close() error {
	if cmd.enc == nil {
		return nil
	}
	cmd.enc.Special(')')
	cmd.enc.end()
	cmd.enc = nil
	return nil
}

func (cmd *AppendCatenateCommand) Wait() (*imap.AppendData, error) {
	return &cmd.data, cmd.wait()
}
