package imapserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// SessionThread is implemented by sessions that support RFC 5256 THREAD.
type SessionThread interface {
	Thread(ctx context.Context, numKind NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error)
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
	if algorithm != imap.ThreadReferences && algorithm != imap.ThreadOrderedSubject && algorithm != imap.ThreadRefs {
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

	data, err := sessionThread.Thread(c.ctx, numKind, algorithm, charset, &criteria)
	if err != nil {
		return err
	}

	return c.writeThread(data)
}

func (c *Conn) writeThread(data []imap.ThreadData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("THREAD")

	var lists []imap.ThreadData
	for _, t := range data {
		if t, ok := normalizeThread(t); ok {
			lists = append(lists, t)
		}
	}

	// thread-data = "THREAD" [SP 1*thread-list] (RFC 5256 section 5): one SP,
	// then the thread-lists back to back. Roundcube reads a THREAD response by
	// matching parentheses and drops every thread after a separating SP.
	if len(lists) > 0 {
		enc.SP()
	}
	for _, t := range lists {
		writeThreadData(enc.Encoder, t)
	}

	return enc.CRLF()
}

// normalizeThread puts data in a form the RFC 5256 section 5 grammar can
// express. A thread-list holds either messages or at least two nested
// thread-lists, so a node with a single sub-thread is written as that
// sub-thread continuing the chain, (1 (2)) as (1 2), and a node with neither
// messages nor sub-threads is left out. ok is false when nothing remains.
func normalizeThread(data imap.ThreadData) (_ imap.ThreadData, ok bool) {
	chain := append([]uint32(nil), data.Chain...)
	var subs []imap.ThreadData
	for _, sub := range data.SubThreads {
		if sub, ok := normalizeThread(sub); ok {
			subs = append(subs, sub)
		}
	}
	for len(subs) == 1 {
		chain = append(chain, subs[0].Chain...)
		subs = subs[0].SubThreads
	}
	if len(chain) == 0 && len(subs) == 0 {
		return imap.ThreadData{}, false
	}
	return imap.ThreadData{Chain: chain, SubThreads: subs}, true
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
