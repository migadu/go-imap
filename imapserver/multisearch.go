package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SessionMultiSearch is an IMAP session that supports the MULTISEARCH extension.
//
// See RFC 7377.
type SessionMultiSearch interface {
	Session
	MultiSearch(numKind NumKind, mailboxes []string, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error)
}

func (c *Conn) handleMultiSearch(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var (
		atom    string
		options imap.SearchOptions
	)

	// RFC 7377 MULTISEARCH [TAG correlator] [RETURN (...)] ("mailbox1" "mailbox2") criteria
	if maybeReadSearchKeyAtom(dec, &atom) && strings.EqualFold(atom, "TAG") {
		var correlator string
		if !dec.ExpectSP() || !dec.ExpectAString(&correlator) || !dec.ExpectSP() {
			return dec.Err()
		}
		tag = correlator
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}

	if strings.EqualFold(atom, "RETURN") {
		if err := readSearchReturnOpts(dec, &options); err != nil {
			return fmt.Errorf("in search-return-opts: %w", err)
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}

	// Read mailbox list
	var mailboxes []string
	if atom != "" {
		// No atom expected here, it must be a list of mailboxes starting with '('
		return fmt.Errorf("expected mailbox list, got atom: %q", atom)
	}

	err := dec.ExpectList(func() error {
		var mbox string
		if !dec.ExpectMailbox(&mbox) {
			return dec.Err()
		}
		mailboxes = append(mailboxes, mbox)
		return nil
	})
	if err != nil {
		return fmt.Errorf("in mailbox list: %w", err)
	}

	if !dec.ExpectSP() {
		return dec.Err()
	}

	maybeReadSearchKeyAtom(dec, &atom)

	if strings.EqualFold(atom, "CHARSET") {
		var charset string
		if !dec.ExpectSP() || !dec.ExpectAString(&charset) || !dec.ExpectSP() {
			return dec.Err()
		}
		switch strings.ToUpper(charset) {
		case "US-ASCII", "UTF-8":
			// nothing to do
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeBadCharset, // TODO: return list of supported charsets
				Text: "Only US-ASCII and UTF-8 are supported SEARCH charsets",
			}
		}
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}

	var criteria imap.SearchCriteria
	for {
		var err error
		if atom != "" {
			err = readSearchKeyWithAtom(c, &criteria, dec, atom)
			atom = ""
		} else {
			err = readSearchKey(c, &criteria, dec)
		}
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

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	// If no return option is specified, ALL is assumed
	if !options.ReturnMin && !options.ReturnMax && !options.ReturnAll && !options.ReturnCount {
		options.ReturnAll = true
	}

	sessionMultiSearch, ok := c.session.(SessionMultiSearch)
	if !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "MULTISEARCH not supported",
		}
	}

	results, err := sessionMultiSearch.MultiSearch(numKind, mailboxes, &criteria, &options)
	if err != nil {
		return err
	}

	for _, data := range results {
		// Force extended format (ESEARCH) for MULTISEARCH
		if err := c.writeESearch(tag, data, &options, numKind); err != nil {
			return err
		}
	}

	return nil
}
