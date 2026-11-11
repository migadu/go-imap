package imapclient_test

import (
	"math"
	"testing"

	"github.com/migadu/go-imap"
)

// A section part is read as a uint32 and kept in an int. On a 32-bit target
// an int cannot hold one above math.MaxInt32, and the conversion wrapped it
// to a negative part. A response with such a part is malformed there; where
// int is wider the part is kept as sent. Issue #179.
func TestFetchSectionPartBeyondInt(t *testing.T) {
	for _, tc := range []struct {
		item string
		part uint64
	}{
		{"BODY[2147483647]", math.MaxInt32},
		{"BODY[2147483648]", math.MaxInt32 + 1},
		{"BODY[4294967295]", math.MaxUint32},
		{"BODY[4294967295.HEADER]", math.MaxUint32},
		{"BINARY[2147483647]", math.MaxInt32},
		{"BINARY[4294967295]", math.MaxUint32},
	} {
		t.Run("FETCH "+tc.item, func(t *testing.T) {
			client := newCodeServer(t, "* 1 FETCH ("+tc.item+" {5}\r\nhello)\r\n")
			client.SetStateForTest(imap.ConnStateSelected)
			msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
				BodySection: []*imap.FetchItemBodySection{{}},
			}).Collect()
			var parts []int
			for _, msg := range msgs {
				for _, s := range msg.BodySection {
					parts = append(parts, s.Section.Part...)
				}
				for _, s := range msg.BinarySection {
					parts = append(parts, s.Section.Part...)
				}
			}
			checkPart(t, tc.item, tc.part, parts, err)
		})
	}
	for _, tc := range []struct {
		part string
		max  uint64
	}{
		{"2147483647", math.MaxInt32},
		{"4294967295", math.MaxUint32},
		{"1.2147483648", math.MaxInt32 + 1},
	} {
		t.Run("FETCH BINARY.SIZE["+tc.part+"]", func(t *testing.T) {
			client := newCodeServer(t, "* 1 FETCH (BINARY.SIZE["+tc.part+"] 5)\r\n")
			client.SetStateForTest(imap.ConnStateSelected)
			msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
				BinarySectionSize: []*imap.FetchItemBinarySectionSize{{}},
			}).Collect()
			var parts []int
			for _, msg := range msgs {
				for _, s := range msg.BinarySectionSize {
					parts = append(parts, s.Part...)
				}
			}
			checkPart(t, "BINARY.SIZE["+tc.part+"]", tc.max, parts, err)
		})
	}
}

// checkPart wants an error for a part beyond int, else the part kept, and
// never a negative part.
func checkPart(t *testing.T, item string, max uint64, parts []int, err error) {
	t.Helper()
	for _, p := range parts {
		if p <= 0 {
			t.Errorf("%s gave the part %d", item, p)
		}
	}
	if max > math.MaxInt {
		if err == nil {
			t.Errorf("%s: parts %v, nil; want an error for a part beyond int", item, parts)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: %v", item, err)
	}
	found := false
	for _, p := range parts {
		found = found || uint64(p) == max
	}
	if !found {
		t.Errorf("%s: parts %v, want %d among them", item, parts, max)
	}
}
