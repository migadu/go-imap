package imapserver

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// SessionCatenate is an IMAP session which supports the CATENATE extension
// (RFC 4469).
type SessionCatenate interface {
	Session

	// AppendCatenate appends to mailbox a message made of parts, in the
	// order parts yields them.
	//
	// The session resolves each URL part itself. RFC 4469 section 3 limits
	// them to relative IMAP URLs (RFC 5092) naming a message or message part
	// the authenticated user can read; the base is the selected mailbox, if
	// any. The octets are copied unchanged and \Seen is not set on the
	// message they come from. A URL the session cannot process is reported
	// with a *BadURLError.
	//
	// The server bounds the octets of the TEXT parts by the APPEND limit.
	// The session bounds the message as a whole, URL parts included, and
	// answers NO with imap.ResponseCodeTooBig beyond its limit.
	AppendCatenate(ctx context.Context, mailbox string, parts CatenateReader, options *imap.AppendOptions) (*imap.AppendData, error)
}

// CatenatePart is one part of a CATENATE append: a URL or a TEXT literal.
type CatenatePart struct {
	// URL is the IMAP URL of a URL part. It is empty for a TEXT part.
	URL string
	// Text holds the octets of a TEXT part. It is nil for a URL part, and
	// valid until the next call to CatenateReader.Next.
	Text imap.LiteralReader
}

// CatenateReader yields the parts of a CATENATE append as the client sends
// them.
type CatenateReader interface {
	// Next returns the next part, or io.EOF after the last one. What is left
	// unread of the previous TEXT part is discarded. Any other error ends
	// the command and should be returned from AppendCatenate.
	Next() (*CatenatePart, error)
}

// BadURLError is returned by SessionCatenate.AppendCatenate for a URL it
// cannot process: bad syntax, a scheme or server it does not serve, an
// invalid message UID or body part (RFC 4469 section 4.1). The server answers
// NO with the BADURL response code.
type BadURLError struct {
	URL string
	// Text is the human-readable text of the response; optional.
	Text string
}

func (err *BadURLError) Error() string {
	return "imapserver: bad CATENATE URL: " + err.URL
}

// maxCatenateParts bounds the parts of one CATENATE append. Each URL part is
// a lookup for the session, and nothing else limits how many a command holds
// short of its size.
const maxCatenateParts = 1000

// handleAppendCatenate handles the extended form of APPEND (RFC 4469 section
// 5), from the SP after "CATENATE":
//
//	append-data =/ "CATENATE" SP "(" cat-part *(SP cat-part) ")"
func (c *Conn) handleAppendCatenate(tag string, dec *imapwire.Decoder, sess SessionCatenate, mailbox string, options *imap.AppendOptions) error {
	if !dec.ExpectSP() || !dec.ExpectSpecial('(') {
		return dec.Err()
	}

	c.setReadTimeout(literalReadTimeout)
	defer c.setReadTimeout(cmdReadTimeout)

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		// As for a plain APPEND, what the client sent of the command is read
		// first. Nothing is appended, so no APPEND limit applies: only what
		// may be read for nothing.
		(&catenateReader{conn: c, dec: dec}).drain()
		return err
	}

	limit := int64(defaultAppendLimit)
	if appendLimitSession, ok := c.session.(SessionAppendLimit); ok {
		limit = int64(appendLimitSession.AppendLimit())
	}

	parts := &catenateReader{conn: c, dec: dec, limit: limit}
	data, appendErr := sess.AppendCatenate(c.ctx, mailbox, parts, options)
	if appendErr == nil && !parts.done && parts.err == nil {
		appendErr = errors.New("imapserver: AppendCatenate returned before the last part")
	}
	if !parts.done {
		// The command has failed, but the client may have sent more of it
		// already. Read that, so the next command starts where it should.
		// What the drain runs into is not reported: the command has failed
		// already, for the reason below.
		parts.drain()
	}
	if parts.err != nil {
		return parts.err
	}
	if appendErr != nil {
		var badURL *BadURLError
		if errors.As(appendErr, &badURL) {
			return badURL.imapError()
		}
		return appendErr
	}

	if err := c.poll("APPEND"); err != nil {
		return err
	}
	return c.writeAppendOK(tag, data)
}

// imapError is the NO response for the URL. RFC 4469 section 5 gives the
// BADURL response code an url-resp-text, "Any TEXT-CHAR except ]"; a URL that
// is not one came from the client and cannot be sent back in the code.
func (err *BadURLError) imapError() *imap.Error {
	imapErr := &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Text: err.Text,
	}
	if imapErr.Text == "" {
		imapErr.Text = "Cannot process this URL"
	}
	if isURLRespText(err.URL) {
		imapErr.Code = imap.ResponseCode(string(imap.ResponseCodeBadURL) + " " + err.URL)
	}
	return imapErr
}

func isURLRespText(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == 0, ch == '\n', ch == '\r', ch == ']', ch == 0xFF:
			return false
		}
	}
	return true
}

// errCatenateStopped ends catenateReader.drain at a synchronizing literal:
// the client has not sent it and, without a continuation request, will not.
var errCatenateStopped = errors.New("imapserver: synchronizing literal in a failed CATENATE")

// catenateReader reads the cat-part list of a CATENATE append.
type catenateReader struct {
	conn *Conn
	dec  *imapwire.Decoder
	// limit bounds the octets of the TEXT parts together; textSize is how
	// many the literals read so far announced.
	limit    int64
	textSize int64
	numParts int
	// lit is the literal of the TEXT part last returned.
	lit *imapwire.LiteralReader
	// done is set once the closing parenthesis and the CRLF have been read.
	done bool
	// draining is set once the command has failed: literals are read and
	// dropped, and none is asked for. stopped is set at a synchronizing
	// literal not asked for: the client sends nothing more.
	draining bool
	stopped  bool
	// err is the first error of the list itself, as opposed to the session's,
	// met before the command failed.
	err error
}

// drainLimit bounds the octets of the TEXT parts once the command has failed:
// the APPEND limit, which the server was prepared to read anyway, or what
// Decoder.DiscardLine reads for nothing, if that is more.
func (r *catenateReader) drainLimit() int64 {
	return max(r.limit, imapwire.MaxDiscardLiteralSize)
}

func (r *catenateReader) Next() (*CatenatePart, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.done {
		return nil, io.EOF
	}
	part, err := r.next()
	if err != nil && err != io.EOF {
		r.err = err
	}
	return part, err
}

// drain reads what is left of a failed command, as far as the client can
// have sent it.
func (r *catenateReader) drain() {
	r.draining = true
	// A URL may come as a literal too. Its octets count with the TEXT parts'
	// against drainLimit.
	checkBuffered := r.dec.CheckBufferedLiteralFunc
	r.dec.CheckBufferedLiteralFunc = func(size int64, nonSync bool) error {
		if !nonSync {
			r.stopped = true
			return errCatenateStopped
		}
		if size > r.drainLimit()-r.textSize {
			r.stopped = true
			r.dec.MarkDesynchronized()
			return errCatenateStopped
		}
		r.textSize += size
		return checkBuffered(size, nonSync)
	}
	defer func() { r.dec.CheckBufferedLiteralFunc = checkBuffered }()
	for !r.done && !r.stopped {
		if _, err := r.next(); err != nil {
			// At a synchronizing literal the client has stopped sending.
			// Past an error of the decoder there is nothing more to parse;
			// Decoder.DiscardLine takes over.
			return
		}
	}
}

func (r *catenateReader) next() (*CatenatePart, error) {
	dec := r.dec

	if lit := r.lit; lit != nil {
		r.lit = nil
		if _, err := io.Copy(io.Discard, lit); err != nil {
			// The client went away mid-literal; see handleAppend.
			return nil, &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Text: "APPEND failed: connection closed before the message was fully received",
			}
		}
	}

	if r.numParts > 0 {
		if dec.Special(')') {
			// The command ends here. Say so only once that is certain, for
			// the session not to store a message a BAD response disowns.
			if !dec.ExpectCRLF() {
				return nil, dec.Err()
			}
			r.done = true
			return nil, io.EOF
		}
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
	}
	if r.numParts >= maxCatenateParts && !r.draining {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeLimit,
			Text: "Too many parts in CATENATE",
		}
	}
	r.numParts++

	var kind string
	if !dec.ExpectAtom(&kind) || !dec.ExpectSP() {
		return nil, dec.Err()
	}
	switch strings.ToUpper(kind) {
	case "URL":
		var url string
		if !dec.ExpectAString(&url) {
			return nil, dec.Err()
		}
		return &CatenatePart{URL: url}, nil
	case "TEXT":
		return r.nextText()
	default:
		return nil, newClientBugError("Unknown CATENATE part type")
	}
}

func (r *catenateReader) nextText() (*CatenatePart, error) {
	dec := r.dec

	tooBig := &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeTooBig,
		Text: "The TEXT parts exceed the APPEND limit",
	}

	// Read only the validated header before deciding whether any payload may
	// be discarded. A failed aggregate budget must not hand the last literal
	// to generic recovery under a fresh per-literal allowance.
	lit, nonSync, err := dec.ExpectLiteralReader()
	if err != nil {
		return nil, err
	}
	if lit.Size() > r.drainLimit()-r.textSize {
		r.stopped = true
		if nonSync {
			dec.MarkDesynchronized()
		}
		return nil, tooBig
	}

	r.textSize += lit.Size()

	if r.draining {
		if !nonSync {
			r.stopped = true
			return nil, errCatenateStopped
		}
		r.lit = lit
		return &CatenatePart{Text: lit}, nil
	}

	if r.textSize > r.limit {
		// Over the APPEND limit, but not over what may be read to drop it:
		// a non-synchronizing literal is read like the parts after it, so
		// the command ends cleanly. A synchronizing one was not sent, and is
		// not asked for.
		if nonSync {
			r.lit = lit
		} else {
			r.stopped = true
		}
		return nil, tooBig
	}

	if err := r.conn.acceptLiteral(lit.Size(), nonSync); err != nil {
		if nonSync {
			// On the wire whatever the answer; within the limit, so read it.
			r.lit = lit
		}
		return nil, err
	}
	r.lit = lit
	return &CatenatePart{Text: lit}, nil
}
