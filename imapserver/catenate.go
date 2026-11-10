package imapserver

import (
	"context"

	"github.com/migadu/go-imap"
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
