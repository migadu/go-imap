package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SessionThread is implemented by sessions that support RFC 5256 THREAD.
type SessionThread interface {
	Thread(numKind NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error)
}

func (c *Conn) handleThread(dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var algStr string
	if !dec.ExpectAtom(&algStr) || !dec.ExpectSP() {
		return dec.Err()
	}

	algorithm := imap.ThreadAlgorithm(strings.ToUpper(algStr))
	if algorithm != imap.ThreadReferences && algorithm != imap.ThreadOrderedSubject {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: fmt.Sprintf("Unsupported THREAD algorithm: %s", algorithm),
		}
	}

	var charset string
	if !dec.ExpectAString(&charset) || !dec.ExpectSP() {
		return dec.Err()
	}
	switch strings.ToUpper(charset) {
	case "US-ASCII", "UTF-8":
		// IMAP4rev2 mandates US-ASCII and UTF-8 support. The backend
		// receives this charset string and can enforce its own restrictions.
	default:
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeBadCharset,
			Text: "Only US-ASCII and UTF-8 are supported THREAD charsets",
		}
	}

	var criteria imap.SearchCriteria
	for {
		var err error
		err = readSearchKey(c, &criteria, dec)
		if err != nil {
			return fmt.Errorf("in search-key: %w", err)
		}

		if !dec.SP() {
			break
		}
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	sessionThread, ok := c.session.(SessionThread)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "THREAD not supported by server",
		}
	}

	data, err := sessionThread.Thread(numKind, algorithm, charset, &criteria)
	if err != nil {
		return err
	}

	return c.writeThread(data)
}

func (c *Conn) writeThread(data []imap.ThreadData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("THREAD")

	for _, t := range data {
		enc.SP()
		writeThreadData(enc.Encoder, t)
	}

	return enc.CRLF()
}

func writeThreadData(enc *imapwire.Encoder, data imap.ThreadData) {
	enc.Special('(')
	for i, num := range data.Chain {
		if i > 0 {
			enc.SP()
		}
		enc.Number(num)
	}
	if len(data.SubThreads) > 0 && len(data.Chain) > 0 {
		enc.SP()
	}
	for _, sub := range data.SubThreads {
		writeThreadData(enc, sub)
	}
	enc.Special(')')
}
