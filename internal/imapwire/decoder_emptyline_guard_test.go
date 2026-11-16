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

type prefixThenErrReader struct {
	prefix string
	pos    int
	reads  int
	err    error
}

func (r *prefixThenErrReader) Read(p []byte) (int, error) {
	r.reads++
	if r.pos < len(r.prefix) {
		n := copy(p, r.prefix[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

func TestDecoderEmptyLineMaxSizeBoundaryOnCRLF(t *testing.T) {
	t.Run("consumes 2-byte CRLF past MaxSize when readBytes == MaxSize", func(t *testing.T) {
		const limit = 10
		raw := strings.Repeat("A", limit) + "\r\n"

		ref := NewDecoder(bufio.NewReader(strings.NewReader(raw)), ConnSideServer)
		ref.MaxSize = limit
		var atom string
		if !ref.ExpectAtom(&atom) {
			t.Fatalf("ref.ExpectAtom failed: %v", ref.Err())
		}
		if ref.CRLF() || !errors.Is(ref.Err(), ErrMaxSize) {
			t.Fatalf("ref.CRLF() = true / err = %v, want false with ErrMaxSize", ref.Err())
		}

		dec := NewDecoder(bufio.NewReader(strings.NewReader(raw)), ConnSideServer)
		dec.MaxSize = limit
		if !dec.ExpectAtom(&atom) {
			t.Fatalf("dec.ExpectAtom failed: %v", dec.Err())
		}
		if dec.EmptyLine() {
			syntax, _ := dec.ReadCount()
			t.Fatalf("EmptyLine() returned true and read %d bytes (MaxSize=%d) with Err()=%v; want false with ErrMaxSize",
				syntax, limit, dec.Err())
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want ErrMaxSize", dec.Err())
		}
	})

	t.Run("consecutive EmptyLine calls enforce MaxSize on second CRLF", func(t *testing.T) {
		dec := NewDecoder(bufio.NewReader(strings.NewReader("\r\n\r\n")), ConnSideServer)
		dec.MaxSize = 2

		if !dec.EmptyLine() {
			t.Fatalf("first EmptyLine() failed: %v", dec.Err())
		}
		if dec.EmptyLine() {
			syntax, _ := dec.ReadCount()
			t.Fatalf("second EmptyLine() succeeded with ReadCount=%d > MaxSize=2 and Err()=%v; want false with ErrMaxSize",
				syntax, dec.Err())
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want ErrMaxSize", dec.Err())
		}
	})

	t.Run("Peek(2) does not read from underlying transport past MaxSize on buffered CR", func(t *testing.T) {
		sentinel := errors.New("read past MaxSize budget")
		src := &prefixThenErrReader{
			prefix: "AAAAAAAAAA\r",
			err:    sentinel,
		}
		dec := NewDecoder(bufio.NewReader(src), ConnSideServer)
		dec.MaxSize = 10

		var atom string
		if !dec.ExpectAtom(&atom) {
			t.Fatalf("ExpectAtom failed: %v", dec.Err())
		}
		if src.reads != 1 {
			t.Fatalf("setup reads = %d, want 1", src.reads)
		}
		_ = dec.EmptyLine()
		if src.reads != 1 {
			t.Fatalf("EmptyLine() performed %d underlying reads (read past MaxSize), want 1; Err()=%v",
				src.reads, dec.Err())
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want %v", dec.Err(), ErrMaxSize)
		}
	})

	t.Run("promotes ErrMaxSize while discarding", func(t *testing.T) {
		dec := NewDecoder(bufio.NewReader(strings.NewReader("\r\n")), ConnSideServer)
		dec.MaxSize = 1
		dec.readBytes = 2
		dec.err = &DecoderExpectError{Message: "prior syntax error"}
		dec.discarding = true

		if dec.EmptyLine() {
			t.Fatal("EmptyLine() succeeded past MaxSize while discarding")
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want ErrMaxSize", dec.Err())
		}
	})
}
