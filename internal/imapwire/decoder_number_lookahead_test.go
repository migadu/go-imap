package imapwire_test

import (
	"strings"
	"testing"
)

// body-fld-lines is a number64, or NIL by a workaround. A number too large for
// it is neither: it must fail as such, not be read as no number and then fail
// on the byte after it, or be taken for the NIL glued to it. Issue #168.
func TestExpectBodyFldLinesOverflow(t *testing.T) {
	for _, s := range []string{"18446744073709551615)", "9223372036854775808)", "9223372036854775808NIL)"} {
		dec := newTestDecoder(s)
		var n int64
		if dec.ExpectBodyFldLines(&n) {
			t.Errorf("ExpectBodyFldLines(%q) = %v, want an error", s, n)
			continue
		}
		expectDecoderExpectError(t, dec)
		if !strings.Contains(dec.Err().Error(), "number too large") {
			t.Errorf("ExpectBodyFldLines(%q) error = %v, want it to name the oversized number", s, dec.Err())
		}
	}

	for s, want := range map[string]int64{"9223372036854775807)": 9223372036854775807, "NIL)": 0, "12)": 12} {
		dec := newTestDecoder(s)
		var n int64
		if !dec.ExpectBodyFldLines(&n) || n != want || !dec.Special(')') {
			t.Errorf("ExpectBodyFldLines(%q) = %v, %v; want %v", s, n, dec.Err(), want)
		}
	}
}
