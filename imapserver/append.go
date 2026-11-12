package imapserver

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
	"github.com/migadu/go-imap/internal/imapwire"
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
	// (APPEND mailbox ~{n}, RFC 3516 §4.4): consume it and skip
	// data-extension parsing. Otherwise an atom here is a data extension
	// such as UTF8 (RFC 6855).
	var dataExt string
	if dec.Special('~') {
		options.Binary = true
	} else if dec.Atom(&dataExt) {
		// Keywords are case-insensitive; normalize once so later comparisons
		// (e.g. the closing ')' check) don't depend on the wire casing.
		dataExt = strings.ToUpper(dataExt)
		switch dataExt {
		case "UTF8":
			// '~' is the literal8 prefix
			if !dec.ExpectSP() || !dec.ExpectSpecial('(') || !dec.ExpectSpecial('~') {
				return dec.Err()
			}
		case "CATENATE":
			sess, ok := c.session.(SessionCatenate)
			if !ok || !c.configuredCaps().Has(imap.CapCatenate) {
				return newClientBugError("Unknown APPEND data extension")
			}
			return c.handleAppendCatenate(tag, dec, sess, mailbox, &options)
		default:
			return newClientBugError("Unknown APPEND data extension")
		}
	}

	lit, nonSync, err := dec.ExpectLiteralReader()
	if err != nil {
		return err
	}
	utf8 := dataExt == "UTF8"

	appendLimit := int64(defaultAppendLimit)
	if appendLimitSession, ok := c.session.(SessionAppendLimit); ok {
		appendLimit = int64(appendLimitSession.AppendLimit())
	}

	// Check authentication state BEFORE accepting the literal.
	// For synchronizing literals, this prevents sending "+ Ready for literal data"
	// to an unauthenticated client. For non-sync literals, the bytes are already
	// on the wire and we must still drain them.
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		c.drainLiteral(lit, dec, nonSync, utf8)
		return err
	}

	// The literal8 APPEND is BINARY's: IMAP4rev2 has only BINARY's FETCH
	// items (RFC 9051 Appendix B).
	if options.Binary && !c.configuredCaps().Has(imap.CapBinary) {
		c.drainLiteral(lit, dec, nonSync, utf8)
		return newClientBugError("APPEND with a literal8 requires BINARY")
	}

	if lit.Size() > appendLimit {
		// For LITERAL+ (non-synchronizing), the client has already sent the literal data.
		// We must drain it from the stream before returning the error, otherwise it will
		// leak into the command parser and cause subsequent commands to fail with errors like:
		// "Subject: BAD Unknown command" or "expected SP, got \"\r\""
		c.drainLiteral(lit, dec, nonSync, utf8)

		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: fmt.Sprintf("Literals are limited to %v bytes for this command", appendLimit),
		}
	}

	if err := c.acceptLiteral(lit.Size(), nonSync); err != nil {
		c.drainLiteral(lit, dec, nonSync, utf8)
		return err
	}

	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)

	body := &appendLiteralReader{lit: lit, dec: dec, remaining: lit.Size(), utf8: utf8}
	// An empty message's backend need not call Read at all, so validate its
	// framing before allowing the backend to commit it.
	if body.Size() == 0 {
		if err := body.finish(); err != nil {
			return err
		}
	}
	data, appendErr := c.session.Append(c.ctx, mailbox, body, &options)
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}
	if appendErr != nil {
		return appendErr
	}
	if err := c.poll("APPEND"); err != nil {
		return err
	}
	return c.writeAppendOK(tag, data)
}

// appendLiteralReader withholds the final octet until the command suffix is
// valid. A backend must not commit a failed APPEND: even a backend that reads
// exactly Size bytes with io.ReadFull must see the framing error before it has
// the complete message. This preserves streaming without buffering the body.
type appendLiteralReader struct {
	lit       *imapwire.LiteralReader
	dec       *imapwire.Decoder
	remaining int64
	utf8      bool
	err       error
}

func (r *appendLiteralReader) Size() int64 { return r.lit.Size() }

func (r *appendLiteralReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.remaining > 1 {
		if int64(len(p)) >= r.remaining {
			p = p[:r.remaining-1]
		}
		n, err := r.lit.Read(p)
		r.remaining -= int64(n)
		if err != nil {
			r.err = err
		}
		return n, err
	}

	var last [1]byte
	if _, err := io.ReadFull(r.lit, last[:]); err != nil {
		r.err = err
		return 0, err
	}
	r.remaining = 0
	if err := r.finish(); err != nil {
		return 0, err
	}
	p[0] = last[0]
	return 1, nil
}

// finish releases the completed literal, then validates the command suffix.
// It is called only once all literal octets have been read.
func (r *appendLiteralReader) finish() error {
	if _, err := io.Copy(io.Discard, r.lit); err != nil {
		r.err = err
		return err
	}
	if err := readAppendEnd(r.dec, r.utf8); err != nil {
		r.err = err
		var syntaxErr *imapwire.DecoderExpectError
		if !errors.As(err, &syntaxErr) {
			// A suffix read can time out after the literal itself completed.
			// Its remaining octets cannot safely become the next command.
			r.dec.MarkDesynchronized()
		}
		return err
	}
	r.err = io.EOF
	return nil
}

func readAppendEnd(dec *imapwire.Decoder, utf8 bool) error {
	if utf8 && !dec.ExpectSpecial(')') {
		return dec.Err()
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	return nil
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
// optional UTF8 closing parenthesis and trailing CRLF so they don't leak into
// the command parser and corrupt the next command. If draining or framing
// fails, the command loop closes the connection
// rather than reporting a recoverable rejection. For synchronizing literals
// it is a no-op — their octets are never sent when the command is rejected
// before the continuation request. The literal read timeout is used because a
// drained payload can be large (e.g. a LITERAL+ message over appendLimit).
func (c *Conn) drainLiteral(lit *imapwire.LiteralReader, dec *imapwire.Decoder, nonSync, utf8 bool) {
	if !nonSync {
		return
	}
	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)
	if _, err := io.Copy(io.Discard, lit); err != nil {
		dec.MarkDesynchronized()
		return
	}
	if readAppendEnd(dec, utf8) != nil {
		dec.MarkDesynchronized()
	}
}
