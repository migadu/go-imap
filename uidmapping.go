package imap

import (
	"fmt"
	"iter"
	"slices"
)

// UIDPairRange pairs two inclusive, ascending ranges of equal length. The nth
// source UID corresponds to the nth destination UID. A single pair has Start ==
// Stop on both sides. Reversed ranges must be normalized before construction.
type UIDPairRange struct {
	Source UIDRange
	Dest   UIDRange
}

// cardinality returns zero for malformed ranges, without unsigned subtraction
// underflow or allowing iteration past the destination's last UID.
func (pair UIDPairRange) cardinality() uint64 {
	if pair.Source.Start == 0 || pair.Dest.Start == 0 ||
		pair.Source.Stop < pair.Source.Start || pair.Dest.Stop < pair.Dest.Start ||
		pair.Source.Stop-pair.Source.Start != pair.Dest.Stop-pair.Dest.Start {
		return 0
	}
	return uint64(pair.Source.Stop) - uint64(pair.Source.Start) + 1
}

// UIDMapping is a COPYUID mapping in copying order (RFC 9051 section 7.1).
// Unlike UIDSet, it never sorts either side independently. Ranges keep memory
// proportional to the wire representation, even for billions of mapped UIDs.
// Each UID must occur at most once on each side; zero and "*" are forbidden.
type UIDMapping []UIDPairRange

// Add appends a pair, merging it with the last range when both UIDs are
// consecutive. Call Validate before using a mapping built from untrusted data.
func (m *UIDMapping) Add(source, dest UID) {
	if len(*m) > 0 {
		last := &(*m)[len(*m)-1]
		if source != 0 && dest != 0 && last.Source.Stop != ^UID(0) && last.Dest.Stop != ^UID(0) &&
			source == last.Source.Stop+1 && dest == last.Dest.Stop+1 {
			last.Source.Stop, last.Dest.Stop = source, dest
			return
		}
	}
	*m = append(*m, UIDPairRange{Source: UIDRange{source, source}, Dest: UIDRange{dest, dest}})
}

// All yields the pairs of a valid mapping in copying order without allocating
// an expanded list. The caller can stop iteration early, including on huge ranges.
// If Cardinality returns zero, All yields no pairs. Use Validate to also check
// for repeated UIDs.
func (m UIDMapping) All() iter.Seq2[UID, UID] {
	return func(yield func(UID, UID) bool) {
		if m.Cardinality() == 0 {
			return
		}
		for _, pair := range m {
			for n := uint64(pair.Source.Start); n <= uint64(pair.Source.Stop); n++ {
				if !yield(UID(n), UID(uint64(pair.Dest.Start)+n-uint64(pair.Source.Start))) {
					return
				}
			}
		}
	}
}

// Cardinality returns the number of pairs in a valid mapping without expanding it.
// It returns zero for malformed ranges or a total exceeding the UID space.
// This check takes O(r) time and no allocations; use Validate to also check
// for repeated UIDs.
func (m UIDMapping) Cardinality() uint64 {
	var count uint64
	for _, pair := range m {
		n := pair.cardinality()
		if n == 0 || count > uint64(^UID(0))-n {
			return 0
		}
		count += n
	}
	return count
}

// Validate rejects empty mappings, invalid ranges, unequal range lengths, and
// repeated UIDs. Validation uses O(r log r) time and O(r) space for r ranges;
// it does not expand ranges into individual UIDs or change copying order.
func (m UIDMapping) Validate() error {
	if len(m) == 0 {
		return fmt.Errorf("imap: empty UID mapping")
	}
	ranges := make([]UIDRange, len(m))
	for i, pair := range m {
		if pair.cardinality() == 0 {
			return fmt.Errorf("imap: invalid UID mapping range %d", i)
		}
	}
	for _, source := range []bool{true, false} {
		for i, pair := range m {
			ranges[i] = pair.Dest
			if source {
				ranges[i] = pair.Source
			}
		}
		slices.SortFunc(ranges, func(a, b UIDRange) int {
			if a.Start < b.Start {
				return -1
			} else if a.Start > b.Start {
				return 1
			}
			return 0
		})
		for i := 1; i < len(ranges); i++ {
			if ranges[i].Start <= ranges[i-1].Stop {
				return fmt.Errorf("imap: repeated UID in mapping")
			}
		}
	}
	return nil
}
