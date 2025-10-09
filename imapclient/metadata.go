package imapclient

import (
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func getMetadataOptionNames(options *imap.GetMetadataOptions) []string {
	if options == nil {
		return nil
	}
	var l []string
	if options.MaxSize != nil {
		l = append(l, "MAXSIZE")
	}
	if options.Depth != imap.GetMetadataDepthZero {
		l = append(l, "DEPTH")
	}
	return l
}

// GetMetadata sends a GETMETADATA command.
//
// This command requires support for the METADATA or METADATA-SERVER extension.
func (c *Client) GetMetadata(mailbox string, entries []string, options *imap.GetMetadataOptions) *GetMetadataCommand {
	// Validate entry names before sending to server
	for _, entry := range entries {
		if err := imap.ValidateMetadataEntry(entry); err != nil {
			cmd := &GetMetadataCommand{mailbox: mailbox}
			cmd.err = fmt.Errorf("invalid entry name %q: %w", entry, err)
			return cmd
		}
	}

	cmd := &GetMetadataCommand{mailbox: mailbox}
	enc := c.beginCommand("GETMETADATA", cmd)
	enc.SP().Mailbox(mailbox)
	if opts := getMetadataOptionNames(options); len(opts) > 0 {
		enc.SP().List(len(opts), func(i int) {
			opt := opts[i]
			enc.Atom(opt).SP()
			switch opt {
			case "MAXSIZE":
				enc.Number(*options.MaxSize)
			case "DEPTH":
				enc.Atom(options.Depth.String())
			default:
				panic(fmt.Errorf("imapclient: unknown GETMETADATA option %q", opt))
			}
		})
	}
	enc.SP().List(len(entries), func(i int) {
		enc.String(entries[i])
	})
	enc.end()
	return cmd
}

// SetMetadata sends a SETMETADATA command.
//
// To remove an entry, set it to nil.
//
// This command requires support for the METADATA or METADATA-SERVER extension.
func (c *Client) SetMetadata(mailbox string, entries map[string]*[]byte) *Command {
	// Validate entry names before sending to server
	for entry := range entries {
		if err := imap.ValidateMetadataEntry(entry); err != nil {
			// Create command that will fail immediately
			cmd := &Command{}
			cmd.err = fmt.Errorf("invalid entry name %q: %w", entry, err)
			return cmd
		}
	}

	cmd := &Command{}
	enc := c.beginCommand("SETMETADATA", cmd)
	enc.SP().Mailbox(mailbox).SP().Special('(')
	i := 0
	for k, v := range entries {
		if i > 0 {
			enc.SP()
		}
		enc.String(k).SP()
		if v == nil {
			enc.NIL()
		} else {
			enc.String(string(*v)) // TODO: use literals if required
		}
		i++
	}
	enc.Special(')')
	enc.end()
	return cmd
}

func (c *Client) handleMetadata() error {
	data, err := readMetadataResp(c.dec)
	if err != nil {
		return fmt.Errorf("in metadata-resp: %v", err)
	}

	cmd := c.findPendingCmdFunc(func(anyCmd command) bool {
		cmd, ok := anyCmd.(*GetMetadataCommand)
		return ok && cmd.mailbox == data.Mailbox
	})
	if cmd != nil && len(data.EntryValues) > 0 {
		cmd := cmd.(*GetMetadataCommand)
		cmd.data.Mailbox = data.Mailbox
		if cmd.data.Entries == nil {
			cmd.data.Entries = make(map[string]*[]byte)
		}
		// The server might send multiple METADATA responses for a single
		// METADATA command
		for k, v := range data.EntryValues {
			cmd.data.Entries[k] = v
		}
	} else if handler := c.options.unilateralDataHandler().Metadata; handler != nil && len(data.EntryList) > 0 {
		handler(data.Mailbox, data.EntryList)
	}

	return nil
}

// GetMetadataCommand is a GETMETADATA command.
type GetMetadataCommand struct {
	commandBase
	mailbox string
	data    imap.GetMetadataData
}

func (cmd *GetMetadataCommand) Wait() (*imap.GetMetadataData, error) {
	return &cmd.data, cmd.wait()
}

type metadataResp struct {
	Mailbox     string
	EntryList   []string
	EntryValues map[string]*[]byte
}

func readMetadataResp(dec *imapwire.Decoder) (*metadataResp, error) {
	var data metadataResp

	if !dec.ExpectMailbox(&data.Mailbox) || !dec.ExpectSP() {
		return nil, dec.Err()
	}

	isList, err := dec.List(func() error {
		var name string
		if !dec.ExpectAString(&name) || !dec.ExpectSP() {
			return dec.Err()
		}

		// TODO: decode as []byte
		var (
			value *[]byte
			s     string
		)
		if dec.String(&s) || dec.Literal(&s) {
			b := []byte(s)
			value = &b
		} else if !dec.ExpectNIL() {
			return dec.Err()
		}

		if data.EntryValues == nil {
			data.EntryValues = make(map[string]*[]byte)
		}
		data.EntryValues[name] = value
		return nil
	})
	if err != nil {
		return nil, err
	} else if !isList {
		var name string
		if !dec.ExpectAString(&name) {
			return nil, dec.Err()
		}
		data.EntryList = append(data.EntryList, name)

		for dec.SP() {
			if !dec.ExpectAString(&name) {
				return nil, dec.Err()
			}
			data.EntryList = append(data.EntryList, name)
		}
	}

	return &data, nil
}
