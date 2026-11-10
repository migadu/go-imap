package imapwire

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
)

// ParseUIDMapping preserves comma order while normalizing the direction of
// each individual range: 12:10 means 10,11,12, not 12,11,10 (RFC 9051).
func ParseUIDMapping(source, dest string) (imap.UIDMapping, error) {
	src, err := parseOrderedUIDSet(source)
	if err != nil {
		return nil, err
	}
	dst, err := parseOrderedUIDSet(dest)
	if err != nil {
		return nil, err
	}
	var mapping imap.UIDMapping
	var i, j int
	var srcOffset, dstOffset uint64
	for i < len(src) && j < len(dst) {
		source, dest := src[i], dst[j]
		srcStart, dstStart := uint64(source.Start)+srcOffset, uint64(dest.Start)+dstOffset
		count := min(uint64(source.Stop)-srcStart+1, uint64(dest.Stop)-dstStart+1)
		mapping = append(mapping, imap.UIDPairRange{
			Source: imap.UIDRange{Start: imap.UID(srcStart), Stop: imap.UID(srcStart + count - 1)},
			Dest:   imap.UIDRange{Start: imap.UID(dstStart), Stop: imap.UID(dstStart + count - 1)},
		})
		srcOffset += count
		dstOffset += count
		if srcStart+count > uint64(source.Stop) {
			i++
			srcOffset = 0
		}
		if dstStart+count > uint64(dest.Stop) {
			j++
			dstOffset = 0
		}
	}
	if i != len(src) || j != len(dst) {
		return nil, fmt.Errorf("imapwire: COPYUID sets have unequal lengths")
	}
	if err := mapping.Validate(); err != nil {
		return nil, err
	}
	return mapping, nil
}

func parseOrderedUIDSet(s string) ([]imap.UIDRange, error) {
	var result []imap.UIDRange
	for item := range strings.SplitSeq(s, ",") {
		startText, stopText, hasRange := strings.Cut(item, ":")
		start, err := strconv.ParseUint(startText, 10, 32)
		if err != nil || start == 0 {
			return nil, fmt.Errorf("imapwire: invalid COPYUID UID")
		}
		stop := start
		if hasRange {
			stop, err = strconv.ParseUint(stopText, 10, 32)
			if err != nil || stop == 0 {
				return nil, fmt.Errorf("imapwire: invalid COPYUID range")
			}
		}
		result = append(result, imap.UIDRange{Start: imap.UID(min(start, stop)), Stop: imap.UID(max(start, stop))})
	}
	return result, nil
}

// UIDMapping writes both UID sequences in copying order, compressing adjacent
// ranges without sorting either side. The mapping must have passed Validate.
func (enc *Encoder) UIDMapping(mapping imap.UIDMapping) *Encoder {
	for _, source := range []bool{true, false} {
		if !source {
			enc.SP()
		}
		var pending imap.UIDRange
		first := true
		flush := func() {
			if pending.Start == 0 {
				return
			}
			if !first {
				enc.Special(',')
			}
			first = false
			enc.Number(uint32(pending.Start))
			if pending.Stop != pending.Start {
				enc.Special(':').Number(uint32(pending.Stop))
			}
		}
		for _, pair := range mapping {
			r := pair.Dest
			if source {
				r = pair.Source
			}
			if pending.Start != 0 && uint64(pending.Stop)+1 == uint64(r.Start) {
				pending.Stop = r.Stop
			} else {
				flush()
				pending = r
			}
		}
		flush()
	}
	return enc
}
