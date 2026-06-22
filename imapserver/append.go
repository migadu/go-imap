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

	// A leading '~' is the literal8 prefix of a bare binary append
	// (APPEND mailbox ~{n}, RFC 3516 / IMAP4rev2): consume it and skip
	// data-extension parsing. Otherwise an atom here is a data extension
	// such as UTF8 (RFC 6855).
	var dataExt string
	if !dec.Special('~') && dec.Atom(&dataExt) {
		// Keywords are case-insensitive; normalize once so later comparisons
		// (e.g. the closing ')' check) don't depend on the wire casing.
		dataExt = strings.ToUpper(dataExt)
		switch dataExt {
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
		c.drainLiteral(lit, dec, nonSync)
		return err
	}

	if lit.Size() > appendLimit {
		// For LITERAL+ (non-synchronizing), the client has already sent the literal data.
		// We must drain it from the stream before returning the error, otherwise it will
		// leak into the command parser and cause subsequent commands to fail with errors like:
		// "Subject: BAD Unknown command" or "expected SP, got \"\r\""
		c.drainLiteral(lit, dec, nonSync)

		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("Literals are limited to %v bytes for this command", appendLimit),
		}
	}

	if err := c.acceptLiteral(lit.Size(), nonSync); err != nil {
		c.drainLiteral(lit, dec, nonSync)
		return err
	}

	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)

	data, appendErr := c.session.Append(mailbox, lit, &options)
	if _, discardErr := io.Copy(io.Discard, lit); discardErr != nil {
		// Draining the unread remainder of the literal failed. lit reads only
		// from the client socket, so this is provably a client-side disconnect
		// — the client/proxy closed or reset the connection mid-upload (a
		// truncated literal, which surfaces as io.ErrUnexpectedEOF), not a
		// server bug. Returning the raw error here routes it through the
		// handler-error path, which logs it as "[SERVERBUG] handling APPEND
		// command: unexpected EOF". Instead return the session's own classified
		// error when it set one, or a plain NO, so it is treated as a normal
		// IMAP response. The reply won't reach a dead socket, but the resulting
		// write failure is recognized as a clean disconnect by the read loop.
		if appendErr != nil {
			return appendErr
		}
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "APPEND failed: connection closed before the message was fully received",
		}
	}
	if dataExt == "UTF8" && !dec.ExpectSpecial(')') {
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

// drainLiteral consumes a rejected non-synchronizing literal's octets and the
// trailing CRLF so they don't leak into the command parser and corrupt the next
// command. It is best-effort: a read error here means the connection is already
// broken, and the caller's protocol error (or the failed response write) is more
// useful than a drain error, so errors are ignored. For synchronizing literals
// it is a no-op — their octets are never sent when the command is rejected
// before the continuation request. The literal read timeout is used because a
// drained payload can be large (e.g. a LITERAL+ message over appendLimit).
func (c *Conn) drainLiteral(lit *imapwire.LiteralReader, dec *imapwire.Decoder, nonSync bool) {
	if !nonSync {
		return
	}
	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)
	_, _ = io.Copy(io.Discard, lit)
	_ = dec.CRLF()
}
