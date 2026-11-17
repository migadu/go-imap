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
		// Skip a structurally closed list before looking for the code bracket:
		// keywords can contain arbitrary '[' and ']' bytes. Even if the outer
		// ']' is absent, keep the tail after ')' so its validation fails rather
		// than rewinding to a bracket inside a keyword. An unclosed list retains
		// the first-bracket recovery below.
		if tail, ok := skipClosedMalformedFlagList(raw); ok {
			p = imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tail+"\r\n")), imapwire.ConnSideClient)
		} else {
			p = newParser()
		}
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

// The bool reports an observed outer ')', not valid flag syntax. The caller
// must still find the response-code bracket and validate the response text.
func skipClosedMalformedFlagList(raw string) (string, bool) {
	s := strings.TrimLeft(raw, " ")
	if len(s) == 0 || s[0] != '(' {
		return "", false
	}
	depth := 1
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
			if i >= len(s) {
				return "", false
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[i+1:], true
			}
		}
	}
	return "", false
}
