package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleGetMetadata(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	// Options are optional and start with ATOM (MAXSIZE/DEPTH)
	// Entries start with astring (typically quoted string)
	var options imap.GetMetadataOptions
	var entries []string
	hasOptions := false

	// Try to parse - could be options list or entry list
	if err := dec.ExpectList(func() error {
		// Check if this is options (starts with atom) or entries (starts with astring)
		var first string
		if dec.Atom(&first) {
			// It's an atom, so this must be options
			hasOptions = true
			firstUpper := strings.ToUpper(first)
			if err := readGetMetadataOption(dec, firstUpper, &options); err != nil {
				return err
			}
			// Continue reading more options if present
			for dec.SP() {
				var optName string
				if !dec.ExpectAtom(&optName) {
					return dec.Err()
				}
				if err := readGetMetadataOption(dec, strings.ToUpper(optName), &options); err != nil {
					return err
				}
			}
			return nil
		} else if dec.String(&first) || dec.Literal(&first) {
			// It's a string, so this is the entry list
			if err := imap.ValidateMetadataEntry(first); err != nil {
				return &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Text: err.Error(),
				}
			}
			entries = append(entries, first)
			// Continue reading more entries
			for dec.SP() {
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
			return nil
		}
		return dec.Err()
	}); err != nil {
		return err
	}

	// If we parsed options, we now need to parse the entry list
	if hasOptions {
		if !dec.ExpectSP() {
			return dec.Err()
		}
		if err := dec.ExpectList(func() error {
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
		}); err != nil {
			return err
		}
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
		return err
	}

	if err := c.writeMetadataResp(data.Mailbox, data.Entries); err != nil {
		return err
	}

	return nil
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

	return session.SetMetadata(mailbox, entries)
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
