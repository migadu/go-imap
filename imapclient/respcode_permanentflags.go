package imapclient

import (
	"bufio"
	"strings"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
	"github.com/migadu/go-imap/internal/imapwire"
)

// readRespCodePermanentFlags captures one bounded physical line before parsing
// its flag list. Keywords can contain ']', so streaming recovery cannot know
// whether that byte closes the response code until the list has been parsed.
// The original decoder retains its reader, byte budget, and unread CRLF.
func readRespCodePermanentFlags(dec *imapwire.Decoder) ([]imap.Flag, string, bool, error) {
	var raw string
	if !dec.ExpectText(&raw) {
		return nil, "", false, dec.Err()
	}
	wire := raw + "\r\n"
	newParser := func() *imapwire.Decoder {
		return imapwire.NewDecoder(bufio.NewReader(strings.NewReader(wire)), imapwire.ConnSideClient)
	}
	p := newParser()
	var flags []imap.Flag
	var valid bool
	if p.SP() {
		flags, valid = internal.ReadFlagList(p)
	}
	if !valid {
		// The list did not close. Recover at the first code bracket on this
		// line, without swallowing it as part of an unfinished keyword.
		p = newParser()
	}
	if !valid || !p.Special(']') {
		// A completed list may contain keyword brackets. Skip trailing junk
		// from after that list, rather than choosing one of those brackets.
		p.DiscardUntilByte(']')
		if !p.ExpectSpecial(']') {
			return nil, "", false, p.Err()
		}
		flags, valid = nil, false
	}
	var text string
	if p.SP() && !p.ExpectText(&text) {
		return nil, "", false, p.Err()
	}
	if !p.ExpectCRLF() {
		return nil, "", false, p.Err()
	}
	return flags, text, valid, nil
}
