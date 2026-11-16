package imapwire

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

type errThenCRLFReader struct {
	calls int
}

func (r *errThenCRLFReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 1 {
		return 0, errors.New("injected transport read failure")
	}
	return copy(p, "\r\n"), nil
}

func TestDecoderEmptyLineHonorsErrorAndLiteralGuards(t *testing.T) {
	t.Run("stops after recorded error without performing a second read", func(t *testing.T) {
		src := &errThenCRLFReader{}
		dec := NewDecoder(bufio.NewReader(src), ConnSideServer)
		_ = dec.EOF()
		if dec.Err() == nil {
			t.Fatal("expected non-EOF read error recorded on decoder")
		}
		if dec.EmptyLine() {
			t.Fatal("EmptyLine() returned true after decoder already failed")
		}
		if src.calls != 1 {
			t.Fatalf("EmptyLine() performed %d reads after recorded error, want 1", src.calls)
		}
	})

	t.Run("rejects EmptyLine while literal is open", func(t *testing.T) {
		dec := NewDecoder(bufio.NewReader(strings.NewReader("{2+}\r\n\r\n")), ConnSideServer)
		lit, _, ok := dec.LiteralReader()
		if !ok {
			t.Fatalf("LiteralReader failed: %v", dec.Err())
		}
		if dec.EmptyLine() {
			t.Fatal("EmptyLine() consumed literal payload bytes while literal was open")
		}
		if dec.Err() == nil {
			t.Fatal("expected EmptyLine() while literal is open to record decoder error")
		}
		buf, err := io.ReadAll(lit)
		if err != nil || string(buf) != "\r\n" {
			t.Fatalf("literal payload = %q, %v, want %q", buf, err, "\r\n")
		}
	})
}
