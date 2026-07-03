package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SessionMultiSearch is an IMAP session that supports the MULTISEARCH extension,
// i.e. the RFC 7377 ESEARCH command with an "IN (source-options)" clause.
//
// See RFC 7377.
type SessionMultiSearch interface {
	Session

	// MultiSearch runs a search across the mailboxes described by source and
	// returns one *imap.SearchData per matching mailbox. The backend resolves the
	// source scopes (personal/subscribed/subtree/... — the server layer cannot
	// enumerate the user's mailboxes) and MUST populate SearchData.Mailbox and
	// SearchData.UIDValidity on every result (RFC 7377 §2.1). Because message
	// numbers are meaningless for unselected mailboxes, results are always UIDs.
	MultiSearch(source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error)
}

// handleESearch implements the RFC 7377 ESEARCH command:
//
//	esearch = "ESEARCH" [SP "IN" SP "(" source-mbox ")"]
//	          [SP search-return-opts] SP search-program
//
// ESEARCH always operates in UID space and every response carries the
// MAILBOX/TAG/UIDVALIDITY correlator. With no IN clause, "selected" is assumed.
func (c *Conn) handleESearch(tag string, dec *imapwire.Decoder) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var (
		atom    string
		options imap.SearchOptions
		source  imap.SearchSource
	)

	// Optional "IN (source-mbox)".
	if maybeReadSearchKeyAtom(dec, &atom) && strings.EqualFold(atom, "IN") {
		if !dec.ExpectSP() {
			return dec.Err()
		}
		if err := readSearchSource(dec, &source); err != nil {
			return fmt.Errorf("in esearch-source-opts: %w", err)
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}

	// Optional "RETURN (...)".
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

	// Optional CHARSET.
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
				Code: imap.ResponseCodeBadCharset,
				Text: "Only US-ASCII and UTF-8 are supported SEARCH charsets",
			}
		}
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}

	// search-program (the search criteria).
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

	// No IN clause ⇒ "selected" is assumed (RFC 7377 §2.1).
	if source.IsZero() {
		source.Selected = true
	}

	// A search limited to the selected mailbox needs one selected; anything wider
	// only needs an authenticated session.
	requiredState := imap.ConnStateAuthenticated
	if source.SelectedOnly() {
		requiredState = imap.ConnStateSelected
	}
	if err := c.checkState(requiredState); err != nil {
		return err
	}

	// If no return option is specified, ALL is assumed.
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

	results, err := sessionMultiSearch.MultiSearch(&source, &criteria, &options)
	if err != nil {
		return err
	}

	for _, data := range results {
		// RFC 7377 §2.1: "An ESEARCH response MUST NOT be returned for mailboxes
		// that contain no matching messages. This is true even when result
		// options such as MIN, MAX, and COUNT are specified."
		if multiSearchDataIsEmpty(data, &options) {
			continue
		}
		if err := c.writeESearch(tag, data, &options, NumKindUID); err != nil {
			return err
		}
	}

	return nil
}

// multiSearchDataIsEmpty reports whether a per-mailbox multi-search result
// matched no messages, in which case RFC 7377 §2.1 forbids emitting an ESEARCH
// response for that mailbox — regardless of which return options were requested.
func multiSearchDataIsEmpty(data *imap.SearchData, options *imap.SearchOptions) bool {
	if options.ReturnMin && data.Min > 0 {
		return false
	}
	if options.ReturnMax && data.Max > 0 {
		return false
	}
	if options.ReturnCount && data.Count > 0 {
		return false
	}
	if options.ReturnAll && data.All != nil && !isNumSetEmpty(data.All) {
		return false
	}
	return true
}

// readSearchSource parses the parenthesized source-mbox of an ESEARCH "IN (...)"
// clause into a SearchSource:
//
//	source-mbox      = filter-mailboxes *(SP filter-mailboxes)
//	filter-mailboxes = "selected" / "inboxes" / "personal" / "subscribed" /
//	                   ("subtree" SP one-or-more-mailbox) /
//	                   ("subtree-one" SP one-or-more-mailbox) /
//	                   ("mailboxes" SP one-or-more-mailbox)
//
// The optional trailing "(scope-options)" group (RFC 7377 §2.1) is not
// implemented — no scope options are defined for SEARCH — and is rejected.
func readSearchSource(dec *imapwire.Decoder, source *imap.SearchSource) error {
	if !dec.ExpectSpecial('(') {
		return dec.Err()
	}

	for {
		var verb string
		if !dec.ExpectAtom(&verb) {
			return dec.Err()
		}

		switch strings.ToLower(verb) {
		case "selected":
			source.Selected = true
		case "inboxes":
			source.Inboxes = true
		case "personal":
			source.Personal = true
		case "subscribed":
			source.Subscribed = true
		case "subtree":
			names, err := readOneOrMoreMailbox(dec)
			if err != nil {
				return err
			}
			source.Subtree = append(source.Subtree, names...)
		case "subtree-one":
			names, err := readOneOrMoreMailbox(dec)
			if err != nil {
				return err
			}
			source.SubtreeOne = append(source.SubtreeOne, names...)
		case "mailboxes":
			names, err := readOneOrMoreMailbox(dec)
			if err != nil {
				return err
			}
			source.Mailboxes = append(source.Mailboxes, names...)
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: fmt.Sprintf("Unknown ESEARCH source option: %q", verb),
			}
		}

		if !dec.SP() {
			break
		}
	}

	if !dec.ExpectSpecial(')') {
		return dec.Err()
	}
	return nil
}

// readOneOrMoreMailbox parses one-or-more-mailbox, which follows the subtree,
// subtree-one and mailboxes filter verbs:
//
//	one-or-more-mailbox = mailbox / "(" mailbox *(SP mailbox) ")"
//
// The leading SP between the verb and the mailbox(es) is consumed here.
func readOneOrMoreMailbox(dec *imapwire.Decoder) ([]string, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}

	if dec.Special('(') {
		var names []string
		for {
			var mbox string
			if !dec.ExpectMailbox(&mbox) {
				return nil, dec.Err()
			}
			names = append(names, mbox)
			if !dec.SP() {
				break
			}
		}
		if !dec.ExpectSpecial(')') {
			return nil, dec.Err()
		}
		return names, nil
	}

	var mbox string
	if !dec.ExpectMailbox(&mbox) {
		return nil, dec.Err()
	}
	return []string{mbox}, nil
}
