package imapclient

import (
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// Copy sends a COPY command.
func (c *Client) Copy(numSet imap.NumSet, mailbox string) *CopyCommand {
	cmd := &CopyCommand{}
	enc := c.beginCommand(uidCmdName("COPY", imapwire.NumSetKind(numSet)), cmd)
	enc.SP().NumSet(numSet).SP().Mailbox(mailbox)
	enc.end()
	return cmd
}

// CopyCommand is a COPY command.
type CopyCommand struct {
	commandBase
	data imap.CopyData
}

func (cmd *CopyCommand) Wait() (*imap.CopyData, error) {
	return &cmd.data, cmd.wait()
}

func readRespCodeCopyUID(dec *imapwire.Decoder) (imap.CopyData, error) {
	// Consume advisory metadata before parsing it: an invalid epoch or missing
	// operand must not poison the decoder or discard the command's status.
	// Leave ']' for the caller and never cross a response boundary looking for
	// it. Transport errors, broken framing, and decoder size limits stay fatal.
	var raw string
	dec.Func(&raw, func(ch byte) bool { return ch != ']' && ch != '\r' && ch != '\n' })
	if err := dec.Err(); err != nil {
		return imap.CopyData{}, err
	}
	fields := strings.Fields(raw)
	if len(fields) != 3 || fields[0][0] < '0' || fields[0][0] > '9' {
		return imap.CopyData{}, nil
	}
	uidValidity, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil || uidValidity == 0 {
		return imap.CopyData{}, nil
	}
	mapping, err := imapwire.ParseUIDMapping(fields[1], fields[2])
	if err != nil {
		return imap.CopyData{}, nil
	}
	return imap.CopyData{UIDValidity: uint32(uidValidity), UIDMapping: mapping}, nil
}
