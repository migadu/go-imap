package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleSort(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var (
		atom     string
		options  imap.SortOptions
		extended bool
	)

	// Check for RETURN options (ESORT)
	if maybeReadSearchKeyAtom(dec, &atom) && strings.EqualFold(atom, "RETURN") {
		if err := readSortReturnOpts(dec, &options); err != nil {
			return fmt.Errorf("in sort-return-opts: %w", err)
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		extended = true
	}

	// Parse sort criteria list: (REVERSE DATE CC FROM)
	var sortCriteria []imap.SortCriterion
	if err := dec.ExpectList(func() error {
		var atom string
		if !dec.ExpectAtom(&atom) {
			return dec.Err()
		}

		atom = strings.ToUpper(atom)
		criterion := imap.SortCriterion{}

		if atom == "REVERSE" {
			criterion.Reverse = true
			if !dec.ExpectSP() || !dec.ExpectAtom(&atom) {
				return dec.Err()
			}
			atom = strings.ToUpper(atom)
		}

		switch atom {
		case "ARRIVAL":
			criterion.Key = imap.SortKeyArrival
		case "CC":
			criterion.Key = imap.SortKeyCc
		case "DATE":
			criterion.Key = imap.SortKeyDate
		case "DISPLAYFROM":
			criterion.Key = imap.SortKeyDisplayFrom
		case "DISPLAYTO":
			criterion.Key = imap.SortKeyDisplayTo
		case "FROM":
			criterion.Key = imap.SortKeyFrom
		case "SIZE":
			criterion.Key = imap.SortKeySize
		case "SUBJECT":
			criterion.Key = imap.SortKeySubject
		case "TO":
			criterion.Key = imap.SortKeyTo
		default:
			return fmt.Errorf("unknown sort key: %s", atom)
		}

		sortCriteria = append(sortCriteria, criterion)
		return nil
	}); err != nil {
		return fmt.Errorf("in sort-criteria: %w", err)
	}

	if !dec.ExpectSP() {
		return dec.Err()
	}

	// Parse charset
	var charset string
	if !dec.ExpectAString(&charset) {
		return dec.Err()
	}

	// Validate charset
	switch strings.ToUpper(charset) {
	case "US-ASCII", "UTF-8":
		// supported charsets
	default:
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeBadCharset,
			Text: "Only US-ASCII and UTF-8 are supported SORT charsets",
		}
	}

	if !dec.ExpectSP() {
		return dec.Err()
	}

	// Parse search criteria (same as SEARCH command)
	var searchCriteria imap.SearchCriteria
	for {
		if err := readSearchKey(c, &searchCriteria, dec); err != nil {
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

	// RETURN after SORT is ESORT's syntax (RFC 5267 §3); RFC 5256 has none, so
	// without ESORT even an empty RETURN () is an argument the server does not
	// support, which gets BAD as for SEARCH (RFC 4466 §2.6.1). The session cannot
	// do this itself, since a plain SORT reaches it as RETURN (ALL) (below).
	if extended && !c.supportsESort() {
		return newClientBugError("SORT RETURN requires ESORT")
	}

	// If no return option is specified, ALL is assumed
	if !options.ReturnMin && !options.ReturnMax && !options.ReturnAll && !options.ReturnCount {
		options.ReturnAll = true
	}

	// Read before the session can rewrite the criteria. readSearchKey keeps a
	// MODSEQ key only when CONDSTORE is supported.
	hasModSeq := searchCriteriaHasModSeq(&searchCriteria)

	// Call the session's Sort method
	data, err := c.session.Sort(c.ctx, numKind, sortCriteria, charset, &searchCriteria, &options)
	if err != nil {
		return err
	}

	// RFC 7162 §3.1.9 and §3.1.10: the response carries the highest
	// mod-sequence of the result only when the criteria use MODSEQ. A client
	// that did not ask cannot be expected to parse it.
	if !hasModSeq {
		data.ModSeq = 0
	}

	// Write SORT/ESEARCH response. Only return options select ESEARCH (RFC 5267
	// §3.1). IMAP4rev2 changes SEARCH's answer (RFC 9051 §6.4.4) but not
	// SORT's, which stays the SORT response of RFC 5256 §4.
	if extended {
		return c.writeESort(tag, data, &options, numKind)
	} else {
		return c.writeSort(data.All, data.ModSeq, numKind)
	}
}

// searchCriteriaHasModSeq reports whether criteria use a MODSEQ key anywhere,
// including under NOT and OR.
func searchCriteriaHasModSeq(criteria *imap.SearchCriteria) bool {
	if criteria.ModSeq != nil {
		return true
	}
	for i := range criteria.Not {
		if searchCriteriaHasModSeq(&criteria.Not[i]) {
			return true
		}
	}
	for i := range criteria.Or {
		if searchCriteriaHasModSeq(&criteria.Or[i][0]) || searchCriteriaHasModSeq(&criteria.Or[i][1]) {
			return true
		}
	}
	return false
}

func (c *Conn) writeSort(nums []uint32, modSeq uint64, numKind NumKind) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("SORT")
	for _, num := range nums {
		enc.SP()
		if numKind == NumKindUID {
			enc.UID(imap.UID(num))
		} else {
			enc.Number(num)
		}
	}
	// RFC 7162 §3.1.9, e.g. "* SORT 2 3 4 (MODSEQ 917162500)". The grammar
	// allows it only after at least one number.
	if modSeq > 0 && len(nums) > 0 {
		enc.SP().Special('(').Atom("MODSEQ").SP().ModSeq(modSeq).Special(')')
	}
	return enc.CRLF()
}

// writeESort answers an extended SORT. RFC 5267 §3.1 gives SORT the return
// options of ESEARCH and answers it with an ESEARCH response, with ALL in sort
// order; no RFC defines an ESORT response.
func (c *Conn) writeESort(tag string, data *imap.SortData, options *imap.SortOptions, numKind NumKind) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("ESEARCH")
	if tag != "" {
		enc.SP().Special('(').Atom("TAG").SP().String(tag).Special(')')
	}
	if numKind == NumKindUID {
		enc.SP().Atom("UID")
	}

	if options.ReturnAll && len(data.All) > 0 {
		enc.SP().Atom("ALL").SP()
		for i, num := range data.All {
			if i > 0 {
				enc.Special(',')
			}
			if numKind == NumKindUID {
				enc.UID(imap.UID(num))
			} else {
				enc.Number(num)
			}
		}
	}
	if options.ReturnMin && data.Min > 0 {
		enc.SP().Atom("MIN").SP().Number(data.Min)
	}
	if options.ReturnMax && data.Max > 0 {
		enc.SP().Atom("MAX").SP().Number(data.Max)
	}
	if options.ReturnCount {
		enc.SP().Atom("COUNT").SP().Number(data.Count)
	}
	// RFC 7162 §3.1.10, e.g. "* ESEARCH (TAG "a") ALL 5,3,2,1 MODSEQ 1236".
	if data.ModSeq > 0 {
		enc.SP().Atom("MODSEQ").SP().ModSeq(data.ModSeq)
	}
	return enc.CRLF()
}

func readSortReturnOpts(dec *imapwire.Decoder, options *imap.SortOptions) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}
	var numRecognizedOpts int
	if err := dec.ExpectList(func() error {
		var name string
		if !dec.ExpectAtom(&name) {
			return dec.Err()
		}
		switch strings.ToUpper(name) {
		case "MIN":
			options.ReturnMin = true
			numRecognizedOpts++
		case "MAX":
			options.ReturnMax = true
			numRecognizedOpts++
		case "ALL":
			options.ReturnAll = true
			numRecognizedOpts++
		case "COUNT":
			options.ReturnCount = true
			numRecognizedOpts++
		default:
			// The extended SORT is "otherwise identical in its behaviour to the
			// extended SEARCH command" (RFC 5267 §3), which rejects options the
			// server does not support with BAD (RFC 4466 §2.6.1). SAVE is
			// SEARCH's only (RFC 5182).
			return newClientBugError(fmt.Sprintf("Unknown SORT return option: %v", name))
		}
		return nil
	}); err != nil {
		return err
	}
	// RFC 5267 §3.3: "If the list of return options is present but empty, then
	// the server provides the ALL return data item"
	if numRecognizedOpts == 0 {
		options.ReturnAll = true
	}
	return nil
}
