package imapwire

import (
	"bufio"
	"strings"
	"testing"
)

// TestDecoderMaxSizeText ensures a long unterminated line is rejected once
// MaxSize is exceeded, rather than buffered unboundedly (hostile-peer DoS).
func TestDecoderMaxSizeText(t *testing.T) {
	input := strings.Repeat("A", 1000) + "\r\n"
	dec := NewDecoder(bufio.NewReader(strings.NewReader(input)), ConnSideClient)
	dec.MaxSize = 10

	var s string
	if dec.Text(&s) {
		t.Fatal("Text() should fail once MaxSize is exceeded")
	}
	if dec.Err() == nil {
		t.Fatal("expected a decoder error after exceeding MaxSize")
	}
}

// TestDecoderResetCount ensures ResetCount makes MaxSize a per-unit budget on a
// reused (long-lived) decoder instead of a cumulative cap.
func TestDecoderResetCount(t *testing.T) {
	input := "AAAAA\r\nBBBBB\r\n"
	dec := NewDecoder(bufio.NewReader(strings.NewReader(input)), ConnSideClient)
	// Enough for one line + CRLF, but not for both lines cumulatively.
	dec.MaxSize = 8

	var s string
	if !dec.Text(&s) || s != "AAAAA" {
		t.Fatalf("first Text() = %q (err=%v); want AAAAA", s, dec.Err())
	}
	if !dec.CRLF() {
		t.Fatalf("CRLF() failed: %v", dec.Err())
	}

	dec.ResetCount()

	if !dec.Text(&s) || s != "BBBBB" {
		t.Fatalf("second Text() after ResetCount = %q (err=%v); want BBBBB", s, dec.Err())
	}
}
