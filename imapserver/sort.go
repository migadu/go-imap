package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

type SortData struct {
	Nums []uint32
}

type SessionSort interface {
	Session

	Sort(numKind NumKind, criteria *imap.SearchCriteria, sortCriteria []imap.SortCriterion) (*SortData, error)
}

func (c *Conn) handleSort(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}

	var sortCriteria []imap.SortCriterion
	listErr := dec.ExpectList(func() error {
		for {
			var criterion imap.SortCriterion
			var atom string
			if !dec.ExpectAtom(&atom) {
				return dec.Err()
			}

			if strings.EqualFold(atom, "REVERSE") {
				criterion.Reverse = true
				if !dec.ExpectSP() || !dec.ExpectAtom(&atom) {
					return dec.Err()
				}
			}

			criterion.Key = imap.SortKey(strings.ToUpper(atom))
			sortCriteria = append(sortCriteria, criterion)

			if !dec.SP() {
				break
			}
		}
		return nil
	})
	if listErr != nil {
		return listErr
	}

	// Parse charset - must be UTF-8 for SORT
	if !dec.ExpectSP() {
		return dec.Err()
	}
	var charset string
	if !dec.ExpectAtom(&charset) || !dec.ExpectSP() {
		return dec.Err()
	}
	if !strings.EqualFold(charset, "UTF-8") {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeBadCharset,
			Text: "Only UTF-8 is supported for SORT",
		}
	}

	// Parse search criteria
	var criteria imap.SearchCriteria
	var atom string
	if maybeReadSearchKeyAtom(dec, &atom) {
		if err := readSearchKeyWithAtom(&criteria, dec, atom); err != nil {
			return fmt.Errorf("in search-key: %w", err)
		}
	} else {
		if err := readSearchKey(&criteria, dec); err != nil {
			return fmt.Errorf("in search-key: %w", err)
		}
	}

	for dec.SP() {
		atom = ""
		if maybeReadSearchKeyAtom(dec, &atom) {
			if err := readSearchKeyWithAtom(&criteria, dec, atom); err != nil {
				return fmt.Errorf("in search-key: %w", err)
			}
		} else {
			if err := readSearchKey(&criteria, dec); err != nil {
				return fmt.Errorf("in search-key: %w", err)
			}
		}
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	var data *SortData
	if sortSession, ok := c.session.(SessionSort); ok {
		var sortErr error
		data, sortErr = sortSession.Sort(numKind, &criteria, sortCriteria)
		if sortErr != nil {
			return sortErr
		}
	} else {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeCannot,
			Text: "SORT not implemented",
		}
	}

	return c.writeSortResponse(data.Nums)
}

func (c *Conn) writeSortResponse(nums []uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("SORT")
	for _, num := range nums {
		enc.SP().Number(num)
	}

	return enc.CRLF()
}
