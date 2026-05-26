package imapserver

import (
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// defaultAppendLimit is the default maximum size of an APPEND payload.
const defaultAppendLimit = 100 * 1024 * 1024 // 100MiB

func (c *Conn) handleAppend(tag string, dec *imapwire.Decoder) error {
	var (
		mailbox string
		options imap.AppendOptions
	)
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	hasFlagList, err := dec.List(func() error {
		flag, err := internal.ExpectFlag(dec)
		if err != nil {
			return err
		}
		options.Flags = append(options.Flags, flag)
		return nil
	})
	if err != nil {
		return err
	}
	if hasFlagList && !dec.ExpectSP() {
		return dec.Err()
	}

	t, err := internal.DecodeDateTime(dec)
	if err != nil {
		return err
	}
	if !t.IsZero() && !dec.ExpectSP() {
		return dec.Err()
	}
	options.Time = t

	var dataExt string
	if !dec.Special('~') && dec.Atom(&dataExt) { // ignore literal8 prefix if any for BINARY
		switch strings.ToUpper(dataExt) {
		case "UTF8":
			// '~' is the literal8 prefix
			if !dec.ExpectSP() || !dec.ExpectSpecial('(') || !dec.ExpectSpecial('~') {
				return dec.Err()
			}
		default:
			return newClientBugError("Unknown APPEND data extension")
		}
	}

	lit, nonSync, err := dec.ExpectLiteralReader()
	if err != nil {
		return err
	}

	appendLimit := int64(defaultAppendLimit)
	if appendLimitSession, ok := c.session.(SessionAppendLimit); ok {
		appendLimit = int64(appendLimitSession.AppendLimit())
	}

	// Check authentication state BEFORE accepting the literal.
	// For synchronizing literals, this prevents sending "+ Ready for literal data"
	// to an unauthenticated client. For non-sync literals, the bytes are already
	// on the wire and we must still drain them.
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		if nonSync {
			io.Copy(io.Discard, lit)
			dec.CRLF()
		}
		return err
	}

	if lit.Size() > appendLimit {
		// For LITERAL+ (non-synchronizing), the client has already sent the literal data.
		// We must drain it from the stream before returning the error, otherwise it will
		// leak into the command parser and cause subsequent commands to fail with errors like:
		// "Subject: BAD Unknown command" or "expected SP, got \"\r\""
		if nonSync {
			// Drain the literal data to prevent it from corrupting the command stream
			if _, err := io.Copy(io.Discard, lit); err != nil {
				return fmt.Errorf("failed to drain oversized literal: %w", err)
			}
			// Consume the CRLF after the literal
			if !dec.CRLF() {
				return dec.Err()
			}
		}

		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("Literals are limited to %v bytes for this command", appendLimit),
		}
	}

	if err := c.acceptLiteral(lit.Size(), nonSync); err != nil {
		return err
	}

	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)

	data, appendErr := c.session.Append(mailbox, lit, &options)
	if _, discardErr := io.Copy(io.Discard, lit); discardErr != nil {
		return discardErr
	}
	if dataExt != "" && !dec.ExpectSpecial(')') {
		return dec.Err()
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	if appendErr != nil {
		return appendErr
	}
	if err := c.poll("APPEND"); err != nil {
		return err
	}
	return c.writeAppendOK(tag, data)
}

func (c *Conn) writeAppendOK(tag string, data *imap.AppendData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom(tag).SP().Atom("OK").SP()
	if data != nil {
		enc.Special('[')
		enc.Atom("APPENDUID").SP().Number(data.UIDValidity).SP().UID(data.UID)
		enc.Special(']').SP()
	}
	enc.Text("APPEND completed")
	return enc.CRLF()
}
