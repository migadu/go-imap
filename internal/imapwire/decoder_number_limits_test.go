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
