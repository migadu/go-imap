package imapmemserver

import (
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// Sort performs a SORT command.
func (mbox *MailboxView) Sort(numKind imapserver.NumKind, criteria *imap.SearchCriteria, sortCriteria []imap.SortCriterion) (*imapserver.SortData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	// Apply search criteria
	mbox.staticSearchCriteria(criteria)

	// First find all messages that match the search criteria
	var matchedMessages []*message
	var matchedSeqNums []uint32
	var matchedIndices []int
	for i, msg := range mbox.l {
		seqNum := mbox.tracker.EncodeSeqNum(uint32(i) + 1)

		if !msg.search(seqNum, criteria) {
			continue
		}

		matchedMessages = append(matchedMessages, msg)
		matchedSeqNums = append(matchedSeqNums, seqNum)
		matchedIndices = append(matchedIndices, i)
	}

	// Sort the matched messages based on the sort criteria
	sortMatchedMessages(matchedMessages, matchedSeqNums, matchedIndices, sortCriteria)

	// Create sorted response
	var data imapserver.SortData
	for i, msg := range matchedMessages {
		var num uint32
		switch numKind {
		case imapserver.NumKindSeq:
			if matchedSeqNums[i] == 0 {
				continue
			}
			num = matchedSeqNums[i]
		case imapserver.NumKindUID:
			num = uint32(msg.uid)
		}
		data.Nums = append(data.Nums, num)
	}

	// Calculate ESORT data fields if there are results
	if len(data.Nums) > 0 {
		// Find min and max values
		min, max := data.Nums[0], data.Nums[0]
		for _, num := range data.Nums {
			if num < min {
				min = num
			}
			if num > max {
				max = num
			}
		}
		data.Min = min
		data.Max = max
	}
	// Set count regardless of whether there are results
	data.Count = uint32(len(data.Nums))

	return &data, nil
}

// sortMatchedMessages sorts messages according to the specified sort criteria
func sortMatchedMessages(messages []*message, seqNums []uint32, indices []int, criteria []imap.SortCriterion) {
	if len(messages) < 2 {
		return // Nothing to sort
	}

	// Create a slice of indices for sorting
	indices2 := make([]int, len(messages))
	for i := range indices2 {
		indices2[i] = i
	}

	// Sort the indices based on the criteria
	sort.SliceStable(indices2, func(i, j int) bool {
		i2, j2 := indices2[i], indices2[j]

		// Apply each criterion in order until we find a difference
		for _, criterion := range criteria {
			result := compareByCriterion(messages[i2], messages[j2], criterion.Key)

			// Apply reverse if needed
			if criterion.Reverse {
				result = -result
			}

			// If comparison yields a difference, return the result
			if result < 0 {
				return true
			} else if result > 0 {
				return false
			}
			// If equal, continue to the next criterion
		}

		// If all criteria are equal, maintain original order
		return i < j
	})

	// Reorder the original slices according to the sorted indices
	newMessages := make([]*message, len(messages))
	newSeqNums := make([]uint32, len(seqNums))
	newIndices := make([]int, len(indices))

	for i, idx := range indices2 {
		newMessages[i] = messages[idx]
		newSeqNums[i] = seqNums[idx]
		newIndices[i] = indices[idx]
	}

	// Copy sorted slices back to original slices
	copy(messages, newMessages)
	copy(seqNums, newSeqNums)
	copy(indices, newIndices)
}

// compareByCriterion compares two messages based on a single criterion
// returns -1 if a < b, 0 if a == b, 1 if a > b
func compareByCriterion(a, b *message, key imap.SortKey) int {
	switch key {
	case imap.SortKeyArrival:
		// For ARRIVAL, we use the UID as the arrival order
		if a.uid < b.uid {
			return -1
		} else if a.uid > b.uid {
			return 1
		}
		return 0

	case imap.SortKeyDate:
		// Compare internal date
		if a.t.Before(b.t) {
			return -1
		} else if a.t.After(b.t) {
			return 1
		}
		return 0

	case imap.SortKeySize:
		// Compare message sizes
		aSize := len(a.buf)
		bSize := len(b.buf)
		if aSize < bSize {
			return -1
		} else if aSize > bSize {
			return 1
		}
		return 0

	case imap.SortKeyFrom:
		// TODO: For a real implementation, extract the From header and compare
		return 0

	case imap.SortKeyTo:
		// TODO: For a real implementation, you would extract the To header and compare
		return 0

	case imap.SortKeyCc:
		// TODO: For a real implementation, you would extract the Cc header and compare
		return 0

	case imap.SortKeySubject:
		// TODO: For a real implementation, you would extract the Subject header and compare
		return 0

	case imap.SortKeyDisplay:
		// SORT=DISPLAY (RFC 5957) - Use a locale-sensitive version of the string
		// For now, treat it the same as the subject sorting for this implementation
		// TODO: For a real implementation, use proper locale-aware sorting of display names
		// A full implementation would handle internationalized text according to
		// the user's locale settings and apply proper collation rules
		return 0

	default:
		// Default to no sorting for unknown criteria
		return 0
	}
}
