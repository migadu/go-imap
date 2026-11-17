package imapmemserver

import (
	"bufio"
	"errors"
	"io"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
	"github.com/migadu/go-imap/internal/imapwire"
)

const maxMIMEHeaderBytes = 1 << 20 // go-message's default root header limit

var errMIMEHeaderTooLarge = errors.New("imapmemserver: MIME header exceeds maximum size")

// readMIMEEntity keeps the root header limit and buffered body bytes while
// normalizing the transfer encoding before either body decoder runs.
func readMIMEEntity(r io.Reader) (*gomessage.Entity, error) {
	lr := &mimeHeaderReader{r: r, remaining: maxMIMEHeaderBytes}
	br := bufio.NewReader(lr)
	h, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, err
	}
	lr.remaining = -1 // the limit applies to the header, not the body
	return newMIMEEntity(gomessage.Header{Header: h}, br)
}

// newMIMEEntity decodes a raw body once. New can apply charset conversion even
// when it cannot decode the CTE, so retrying New on Entity.Body is not safe.
func newMIMEEntity(header gomessage.Header, body io.Reader) (*gomessage.Entity, error) {
	decodeHeader := header
	rawCTE := header.Get("Content-Transfer-Encoding")
	mech := imapwire.CTEMechanism(rawCTE)
	switch imapwire.ToLowerASCII(mech) {
	case "", "7bit", "8bit", "binary", "base64", "quoted-printable":
		if mech != rawCTE {
			decodeHeader = header.Copy()
			decodeHeader.Set("Content-Transfer-Encoding", mech)
		}
	}
	e, err := gomessage.New(decodeHeader, body)
	if e != nil {
		// Raw-token checks and SEARCH TEXT must see the original field values.
		e.Header = header
	}
	return e, err
}

type mimeHeaderReader struct {
	r         io.Reader
	remaining int64 // negative after the header has been read
}

func (r *mimeHeaderReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, errMIMEHeaderTooLarge
	}
	if r.remaining > 0 && int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	if r.remaining > 0 {
		r.remaining -= int64(n)
	}
	return n, err
}
