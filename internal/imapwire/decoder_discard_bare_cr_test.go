package imapwire

import (
	"bufio"
	"strings"
	"testing"
)

// A bare CR is not a command boundary. Recovery from an invalid literal must
// not expose the rest of the same physical line as another command.
func TestMalformedLiteralBareCRStopsRecovery(t *testing.T) {
	for _, prefix := range []string{
		"{x}",
		"{3+}\r",
		"{3+} ",
		"{-1+}",
		"{3+x}",
	} {
		t.Run(prefix, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(prefix + "\rhidden NOOP\r\nreal NOOP\r\n"))
			dec := NewDecoder(r, ConnSideServer)
			if _, _, ok := dec.LiteralReader(); ok {
				t.Fatal("malformed literal was accepted")
			}
			expectErrorRecorded(t, dec)
			dec.DiscardLine()
			if !dec.Desynchronized() {
				next := NewDecoder(r, ConnSideServer)
				var tag, cmd string
				if next.ExpectAtom(&tag) && next.ExpectSP() && next.ExpectAtom(&cmd) && next.ExpectCRLF() {
					t.Fatalf("recovery left same-line input executable as %q %q", tag, cmd)
				}
				t.Fatal("failed line ending left stream eligible for recovery")
			}
			remaining := r.Buffered()
			dec.DiscardLine()
			if r.Buffered() != remaining {
				t.Fatal("repeated recovery consumed input after framing became uncertain")
			}
		})
	}
}

// A complete malformed line can still be answered BAD and followed by another
// command. In particular, the extra CR is consumed as part of the failed
// announcement, leaving an actual CRLF for DiscardLine to consume.
func TestMalformedLiteralCompleteLineStillRecovers(t *testing.T) {
	for _, prefix := range []string{
		"{x}",
		"{3+}\r",
		"{3+} ",
		"{-1+}",
		"{3+x}",
	} {
		t.Run(prefix, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(prefix + "\r\nreal NOOP\r\n"))
			dec := NewDecoder(r, ConnSideServer)
			if _, _, ok := dec.LiteralReader(); ok {
				t.Fatal("malformed literal was accepted")
			}
			expectErrorRecorded(t, dec)
			dec.DiscardLine()
			if dec.Desynchronized() {
				t.Fatal("complete malformed line prevented recovery")
			}
			next := NewDecoder(r, ConnSideServer)
			var tag, cmd string
			if !next.ExpectAtom(&tag) || !next.ExpectSP() || !next.ExpectAtom(&cmd) || !next.ExpectCRLF() {
				t.Fatalf("following command failed: %v", next.Err())
			}
			if tag != "real" || cmd != "NOOP" {
				t.Fatalf("following command = %q %q, want real NOOP", tag, cmd)
			}
		})
	}
}
