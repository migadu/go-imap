package imapwire_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap/internal/imapwire"
)

func TestDecoderNumberLimits(t *testing.T) {
	for _, tc := range []struct {
		name, maximum, overflow string
		read                    func(*imapwire.Decoder, bool) bool
	}{
		{"number", "4294967295", "4294967296", func(dec *imapwire.Decoder, expect bool) bool {
			var n uint32
			if expect {
				return dec.ExpectNumber(&n)
			}
			return dec.Number(&n)
		}},
		{"number64", "9223372036854775807", "9223372036854775808", func(dec *imapwire.Decoder, expect bool) bool {
			var n int64
			if expect {
				return dec.ExpectNumber64(&n)
			}
			return dec.Number64(&n)
		}},
		{"modseq", "18446744073709551615", "18446744073709551616", func(dec *imapwire.Decoder, expect bool) bool {
			var n uint64
			if expect {
				return dec.ExpectModSeq(&n)
			}
			return dec.ModSeq(&n)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := newTestDecoder(tc.maximum + ")")
			if !tc.read(dec, true) || dec.Err() != nil || !dec.Special(')') {
				t.Fatalf("maximum value rejected: %v", dec.Err())
			}

			dec = newTestDecoder(tc.overflow + ")")
			if tc.read(dec, true) {
				t.Fatal("overflow accepted")
			}
			expectDecoderExpectError(t, dec)
			if !strings.Contains(dec.Err().Error(), "number too large") {
				t.Fatalf("overflow error = %v", dec.Err())
			}

			// Optional forms intentionally leave the decoder usable, including
			// the closing delimiter, for unknown response-code tolerance.
			dec = newTestDecoder(tc.overflow + ")")
			if tc.read(dec, false) || dec.Err() != nil || !dec.Special(')') {
				t.Fatalf("optional overflow poisoned the decoder: %v", dec.Err())
			}
		})
	}
}

// mod-sequence-value is "Positive unsigned 63-bit integer ...
// (1 <= n <= 9,223,372,036,854,775,807)" (RFC 7162 Section 7).
func TestDecoderModSequenceValue(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64 // 0: refused
	}{
		{"1)", 1},
		{"9223372036854775807)", 9223372036854775807},
		{"0)", 0},
		{"00)", 0},
		{"9223372036854775808)", 0},
	} {
		dec := newTestDecoder(tc.in)
		var v uint64
		ok := dec.ExpectModSequenceValue(&v)
		if tc.want == 0 {
			if ok {
				t.Errorf("%q: accepted as %v", tc.in, v)
				continue
			}
			expectDecoderExpectError(t, dec)
			continue
		}
		if !ok || v != tc.want || dec.Err() != nil || !dec.Special(')') {
			t.Errorf("%q: got %v, %v (err %v), want %v", tc.in, v, ok, dec.Err(), tc.want)
		}
	}
}
