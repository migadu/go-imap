package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleGetMetadata(tag string, dec *imapwire.Decoder) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	// RFC 5464: "GETMETADATA" [SP getmetadata-options] SP mailbox SP entries
	var options imap.GetMetadataOptions
	var entries []string
	hasOptions := false

	// Check for optional options list
	_, err := dec.List(func() error {
		// Parse options: MAXSIZE <number> or DEPTH <0|1|infinity>
		var optName string
		if !dec.ExpectAtom(&optName) {
			return dec.Err()
		}
		if err := readGetMetadataOption(dec, strings.ToUpper(optName), &options); err != nil {
			return err
		}
		hasOptions = true
		return nil
	})
	if err != nil {
		return err
	}

	// Read mailbox
	if hasOptions {
		if !dec.ExpectSP() {
			return dec.Err()
		}
	}
	var mailbox string
	if !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	// Parse entries: single entry or list
	isList, err := dec.List(func() error {
		var entry string
		if !dec.ExpectAString(&entry) {
			return dec.Err()
		}
		if err := imap.ValidateMetadataEntry(entry); err != nil {
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: err.Error(),
			}
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return err
	}

	// If not a list, parse single entry
	if !isList {
		var entry string
		if !dec.ExpectAString(&entry) {
			return dec.Err()
		}
		if err := imap.ValidateMetadataEntry(entry); err != nil {
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: err.Error(),
			}
		}
		entries = append(entries, entry)
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionMetadata)
	if !ok {
		return newClientBugError("GETMETADATA is not supported")
	}

	opts := &options
	if !hasOptions {
		opts = nil
	}

	data, err := session.GetMetadata(mailbox, entries, opts)
	if err != nil {
		return fmt.Errorf("GETMETADATA for mailbox %q: %w", mailbox, err)
	}

	if err := c.writeMetadataResp(data.Mailbox, data.Entries); err != nil {
		return fmt.Errorf("writing METADATA response for mailbox %q: %w", mailbox, err)
	}

	return c.writeGetMetadataOK(tag, data)
}

func (c *Conn) writeGetMetadataOK(tag string, data *imap.GetMetadataData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom(tag).SP().Atom("OK").SP()
	if data.LongEntries > 0 {
		enc.Special('[').Atom("METADATA").SP().Atom("LONGENTRIES").SP().Number(data.LongEntries).Special(']').SP()
	}
	enc.Text("GETMETADATA completed")
	return enc.CRLF()
}

func (c *Conn) handleSetMetadata(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	// Parse entry-value list
	entries := make(map[string]*[]byte)
	if err := dec.ExpectList(func() error {
		var entry string
		if !dec.ExpectAString(&entry) || !dec.ExpectSP() {
			return dec.Err()
		}

		if err := imap.ValidateMetadataEntry(entry); err != nil {
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: err.Error(),
			}
		}

		var value *[]byte
		var s string
		if dec.String(&s) || dec.Literal(&s) {
			b := []byte(s)
			value = &b
		} else if !dec.ExpectNIL() {
			return dec.Err()
		}

		entries[entry] = value
		return nil
	}); err != nil {
		return err
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionMetadata)
	if !ok {
		return newClientBugError("SETMETADATA is not supported")
	}

	if err := session.SetMetadata(mailbox, entries); err != nil {
		return fmt.Errorf("SETMETADATA for mailbox %q: %w", mailbox, err)
	}

	return nil
}

func (c *Conn) writeMetadataResp(mailbox string, entries map[string]*[]byte) error {
	if len(entries) == 0 {
		return nil
	}

	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("METADATA").SP().Mailbox(mailbox).SP()
	listEnc := enc.BeginList()
	for entry, value := range entries {
		listEnc.Item().String(entry).SP()
		if value == nil {
			enc.NIL()
		} else {
			enc.String(string(*value))
		}
	}
	listEnc.End()

	return enc.CRLF()
}

func readGetMetadataOption(dec *imapwire.Decoder, name string, options *imap.GetMetadataOptions) error {
	switch name {
	case "MAXSIZE":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var maxSize uint32
		if !dec.ExpectNumber(&maxSize) {
			return dec.Err()
		}
		options.MaxSize = &maxSize
	case "DEPTH":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var depthStr string
		if !dec.ExpectAtom(&depthStr) {
			return dec.Err()
		}
		switch strings.ToLower(depthStr) {
		case "0":
			options.Depth = imap.GetMetadataDepthZero
		case "1":
			options.Depth = imap.GetMetadataDepthOne
		case "infinity":
			options.Depth = imap.GetMetadataDepthInfinity
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: fmt.Sprintf("Invalid DEPTH value: %s", depthStr),
			}
		}
	default:
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: fmt.Sprintf("Unknown GETMETADATA option: %s", name),
		}
	}
	return nil
}
