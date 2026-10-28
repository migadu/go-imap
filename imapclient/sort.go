package imapclient

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapnum"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// SortOptions contains options for the SORT command.
type SortOptions struct {
	// The search criteria for the messages to sort.
	SearchCriteria *imap.SearchCriteria
	// A list of criteria to sort by.
	SortCriteria []imap.SortCriterion
	// Return options for ESORT. If any are set, an extended SORT is used.
	Return imap.SortOptions
}

// SortData is the data returned by a SORT or ESORT command.
type SortData struct {
	// A list of matching message numbers, in sorted order.
	// Populated if Return.All is true (default for SORT).
	// Either SeqNums or UIDs is populated.
	SeqNums []uint32
	UIDs    []imap.UID

	// The following fields are only populated for ESORT.
	Min   uint32
	Max   uint32
	Count uint32

	// The highest mod-sequence of the returned messages. Populated when the
	// search criteria use MODSEQ (RFC 7162 §3.1.9, §3.1.10).
	ModSeq uint64 // requires CONDSTORE
}

func (c *Client) sort(numKind imapwire.NumKind, options *SortOptions) *SortCommand {
	cmd := &SortCommand{numKind: numKind}
	enc := c.beginCommand(uidCmdName("SORT", numKind), cmd)

	isESort := options.Return.ReturnMin || options.Return.ReturnMax || options.Return.ReturnAll || options.Return.ReturnCount
	if isESort {
		enc.SP().Atom("RETURN").SP()
		var returnOpts []string
		if options.Return.ReturnMin {
			returnOpts = append(returnOpts, "MIN")
		}
		if options.Return.ReturnMax {
			returnOpts = append(returnOpts, "MAX")
		}
		if options.Return.ReturnAll {
			returnOpts = append(returnOpts, "ALL")
		}
		if options.Return.ReturnCount {
			returnOpts = append(returnOpts, "COUNT")
		}
		enc.List(len(returnOpts), func(i int) {
			enc.Atom(returnOpts[i])
		})
	}

	enc.SP().List(len(options.SortCriteria), func(i int) {
		criterion := options.SortCriteria[i]
		if criterion.Reverse {
			enc.Atom("REVERSE").SP()
		}
		enc.Atom(string(criterion.Key))
	})
	enc.SP().Atom("UTF-8").SP()
	writeSearchKey(enc.Encoder, options.SearchCriteria, c.Caps().Has(imap.CapCondStore))
	enc.end()
	return cmd
}

func (c *Client) handleSort() error {
	cmd := findPendingCmdByType[*SortCommand](c)
	for c.dec.SP() {
		if c.dec.Special('(') {
			// RFC 7162 §3.1.9: "* SORT 2 3 4 (MODSEQ 917162500)"
			var name string
			if !c.dec.ExpectAtom(&name) || !c.dec.ExpectSP() {
				return c.dec.Err()
			} else if strings.ToUpper(name) != "MODSEQ" {
				return fmt.Errorf("in search-sort-mod-seq: expected %q, got %q", "MODSEQ", name)
			}
			var modSeq uint64
			if !c.dec.ExpectModSeq(&modSeq) || !c.dec.ExpectSpecial(')') {
				return c.dec.Err()
			}
			if cmd != nil {
				cmd.data.ModSeq = modSeq
			}
			break
		}

		var num uint32
		if !c.dec.ExpectNumber(&num) {
			return c.dec.Err()
		}
		if cmd != nil {
			if cmd.numKind == imapwire.NumKindSeq {
				cmd.data.SeqNums = append(cmd.data.SeqNums, num)
			} else {
				cmd.data.UIDs = append(cmd.data.UIDs, imap.UID(num))
			}
		}
	}
	return nil
}

// handleESort reads an untagged ESORT response. No RFC defines one: an extended
// SORT is answered with ESEARCH (RFC 5267 §3.1), which handleESearch routes to
// readESortData. Servers built from this fork sent ESORT before they were
// fixed, so it is still accepted.
func (c *Client) handleESort() error {
	cmd := findPendingCmdByType[*SortCommand](c)
	if cmd == nil {
		// This is an unsolicited ESORT response, parse and discard its parameters
		for c.dec.SP() {
			if !c.dec.DiscardValue() {
				return c.dec.Err()
			}
		}
		return nil
	}
	return c.readESortData(cmd)
}

// readESortData reads the return data of an extended SORT into cmd.
func (c *Client) readESortData(cmd *SortCommand) error {
	isUID := cmd.numKind == imapwire.NumKindUID

	for c.dec.SP() {
		var s string
		if c.dec.Special('(') {
			var key, tag string
			if !c.dec.ExpectAtom(&key) || !strings.EqualFold(key, "TAG") || !c.dec.ExpectSP() || !c.dec.ExpectAString(&tag) || !c.dec.ExpectSpecial(')') {
				return c.dec.Err()
			}
			continue
		}

		if !c.dec.ExpectAtom(&s) {
			return c.dec.Err()
		}

		switch strings.ToUpper(s) {
		case "UID":
			isUID = true
		case "MIN":
			if !c.dec.ExpectSP() || !c.dec.ExpectNumber(&cmd.data.Min) {
				return c.dec.Err()
			}
		case "MAX":
			if !c.dec.ExpectSP() || !c.dec.ExpectNumber(&cmd.data.Max) {
				return c.dec.Err()
			}
		case "COUNT":
			if !c.dec.ExpectSP() || !c.dec.ExpectNumber(&cmd.data.Count) {
				return c.dec.Err()
			}
		case "MODSEQ": // RFC 7162 §3.1.10
			if !c.dec.ExpectSP() || !c.dec.ExpectModSeq(&cmd.data.ModSeq) {
				return c.dec.Err()
			}
		case "ALL":
			var seqSetStr string
			if !c.dec.ExpectSP() || !c.dec.ExpectAtom(&seqSetStr) {
				return c.dec.Err()
			}
			nums, err := sortedNums(seqSetStr)
			if err != nil {
				return fmt.Errorf("in ALL seq-set: %w", err)
			}

			if isUID {
				cmd.data.UIDs = make([]imap.UID, len(nums))
				for i, n := range nums {
					cmd.data.UIDs[i] = imap.UID(n)
				}
			} else {
				cmd.data.SeqNums = nums
			}
		default:
			// search-return-data is extensible (RFC 4466 §2.6.2), and a real
			// ESEARCH can carry more than SORT asked for, e.g. MODSEQ (RFC 7162
			// §3.1.10). Skip it, as readESearchReturnData does.
			if !c.dec.ExpectSP() || !c.dec.DiscardValue() {
				return c.dec.Err()
			}
		}
	}

	return nil
}

// sortedNums expands the ALL sequence-set of an extended SORT. RFC 5267 §3.1
// lists the numbers in sort order, so the elements are expanded in the order
// given rather than through imapnum.Set, which sorts them. A range stands for
// its numbers in increasing order (§3.2).
func sortedNums(s string) ([]uint32, error) {
	const maxAllResultSize = 10_000_000
	var nums []uint32
	for elem := range strings.SplitSeq(s, ",") {
		r, err := imapnum.ParseRange(elem)
		if err != nil {
			return nil, err
		}
		if r.Start == 0 || r.Stop == 0 {
			return nil, fmt.Errorf("esort: ALL contained a dynamic set, which is not allowed")
		}
		if uint64(len(nums))+uint64(r.Stop-r.Start)+1 > maxAllResultSize {
			return nil, fmt.Errorf("esort: server returned ALL set with more than %d entries", maxAllResultSize)
		}
		for n := r.Start; ; n++ {
			nums = append(nums, n)
			if n == r.Stop {
				break
			}
		}
	}
	return nums, nil
}

// Sort sends a SORT command.
//
// This command requires support for the SORT extension.
func (c *Client) Sort(options *SortOptions) *SortCommand {
	return c.sort(imapwire.NumKindSeq, options)
}

// UIDSort sends a UID SORT command.
//
// See Sort.
func (c *Client) UIDSort(options *SortOptions) *SortCommand {
	return c.sort(imapwire.NumKindUID, options)
}

// SortCommand is a SORT command.
type SortCommand struct {
	commandBase
	numKind imapwire.NumKind
	data    SortData
}

func (cmd *SortCommand) Wait() (*SortData, error) {
	err := cmd.wait()
	return &cmd.data, err
}
