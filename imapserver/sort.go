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

	// If no return option is specified, ALL is assumed
	if !options.ReturnMin && !options.ReturnMax && !options.ReturnAll && !options.ReturnCount {
		options.ReturnAll = true
	}

	// Call the session's Sort method
	data, err := c.session.Sort(numKind, sortCriteria, charset, &searchCriteria, &options)
	if err != nil {
		return err
	}

	// Write SORT/ESORT response
	if c.enabled.Has(imap.CapIMAP4rev2) || extended {
		return c.writeESort(tag, data, &options, numKind)
	} else {
		return c.writeSort(data.All, numKind)
	}
}

func (c *Conn) writeSort(nums []uint32, numKind NumKind) error {
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
	return enc.CRLF()
}

func (c *Conn) writeESort(tag string, data *imap.SortData, options *imap.SortOptions, numKind NumKind) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("ESORT")
	if tag != "" {
		enc.SP().Special('(').Atom("TAG").SP().Atom(tag).Special(')')
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
			// RFC 5267: Servers MUST ignore any unknown sort-return-opt
		}
		return nil
	}); err != nil {
		return err
	}
	// RFC 5267: If the list of return options is present but empty (or only has unknown options),
	// then the server provides the ALL return data item
	if numRecognizedOpts == 0 {
		options.ReturnAll = true
	}
	return nil
}
