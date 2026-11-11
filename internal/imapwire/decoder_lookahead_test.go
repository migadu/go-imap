package imapwire

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

func TestNextBytesFold(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{input: " (LAZY)\r\n", want: true},
		{input: " (lazy)\r\n", want: true},
		{input: " (LaZy ", want: true},
		{input: " (LAZ\r\n", want: false},
		{input: " (CHANGEDSINCE 1)\r\n", want: false},
		{input: "\r\n", want: false},
		{input: ")\r\n", want: false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			dec := NewDecoder(bufio.NewReader(strings.NewReader(tc.input)), ConnSideServer)
			if got := dec.NextBytesFold(" (LAZY"); got != tc.want {
				t.Errorf("NextBytesFold() = %v, want %v", got, tc.want)
			}
			if err := dec.Err(); err != nil {
				t.Errorf("Err() = %v", err)
			}
			// Nothing is consumed either way.
			rest, _ := io.ReadAll(dec.r)
			if string(rest) != tc.input {
				t.Errorf("left %q, want %q", rest, tc.input)
			}
		})
	}
}

// TestNextBytesFoldDoesNotWaitPastAMismatch checks that the lookahead returns
// as soon as one byte rules the pattern out, instead of waiting for as many
// bytes as the pattern is long: a command line can be shorter than that.
func TestNextBytesFoldDoesNotWaitPastAMismatch(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte("\r\n")) // the line ends; no more bytes will come

	dec := NewDecoder(bufio.NewReader(r), ConnSideServer)
	done := make(chan bool, 1)
	go func() { done <- dec.NextBytesFold(" (LAZY") }()
	select {
	case got := <-done:
		if got {
			t.Errorf("NextBytesFold() = true, want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("NextBytesFold() is still waiting for input")
	}
}

func TestNextBytesFoldAtEOF(t *testing.T) {
	dec := NewDecoder(bufio.NewReader(strings.NewReader(" (")), ConnSideServer)
	if dec.NextBytesFold(" (LAZY") {
		t.Errorf("NextBytesFold() = true at EOF")
	}
	if dec.Err() == nil {
		t.Errorf("Err() = nil, want the read error")
	}
}
