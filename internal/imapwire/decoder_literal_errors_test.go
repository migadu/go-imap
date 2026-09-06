package imapwire

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func newServerDecoder(input string) *Decoder {
	return NewDecoder(bufio.NewReader(strings.NewReader(input)), ConnSideServer)
}

func expectErrorRecorded(t *testing.T, dec *Decoder) {
	t.Helper()
	var expectErr *DecoderExpectError
	if err := dec.Err(); !errors.As(err, &expectErr) {
		t.Fatalf("recorded error is %T (%v); want *DecoderExpectError", err, err)
	}
}

// Without a CheckBufferedLiteralFunc the decoder applies its own cap on
// literals it must buffer. Exceeding it is the peer's syntax, so the recorded
// error is a DecoderExpectError -- the type a server maps to BAD -- and not a
// plain error that reads as an internal failure.
func TestLiteralDefaultCapIsExpectError(t *testing.T) {
	dec := newServerDecoder("{5000000}\r\n")
	var s string
	if dec.Literal(&s) {
		t.Fatal("Literal accepted a literal over the default cap")
	}
	expectErrorRecorded(t, dec)
}

// A literal size beyond the sanity bound is refused on sight. The refusal is a
// syntax error, and what it does to the stream depends on the literal's kind:
// a synchronizing literal was never sent, so the connection recovers; a
// non-synchronizing one has an unvalidated quantity of octets in flight, so the
// stream cannot be resynchronised.
func TestLiteralSizeOutOfBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		desynced bool
	}{
		{"synchronizing", "{99999999999}\r\n", false},
		{"non-synchronizing", "{99999999999+}\r\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := newServerDecoder(tc.input)
			if _, _, ok := dec.LiteralReader(); ok {
				t.Fatal("LiteralReader accepted a size beyond the bound")
			}
			expectErrorRecorded(t, dec)
			dec.DiscardLine()
			if got := dec.Desynchronized(); got != tc.desynced {
				t.Errorf("Desynchronized() = %v, want %v", got, tc.desynced)
			}
		})
	}
}
