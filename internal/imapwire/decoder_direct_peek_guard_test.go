package imapwire

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type chunksThenErrReader struct {
	chunks []string
	errIdx int
	err    error
	calls  int
}

func (r *chunksThenErrReader) Read(p []byte) (int, error) {
	idx := r.calls
	r.calls++
	if idx == r.errIdx {
		return 0, r.err
	}
	if idx > r.errIdx {
		idx--
	}
	if idx < len(r.chunks) {
		return copy(p, r.chunks[idx]), nil
	}
	return 0, io.EOF
}

func TestDecoderNextBytesFoldMaxSize(t *testing.T) {
	t.Run("fails with ErrMaxSize when readBytes already exceeds MaxSize", func(t *testing.T) {
		sentinel := errors.New("read past MaxSize budget")
		src := &prefixThenErrReader{
			prefix: "\r\n  (LAZY)",
			err:    sentinel,
		}
		dec := NewDecoder(bufio.NewReaderSize(src, 16), ConnSideServer)
		dec.MaxSize = 2

		if !dec.EmptyLine() {
			t.Fatalf("EmptyLine() failed: %v", dec.Err())
		}
		if !dec.Special(' ') {
			t.Fatalf("Special(' ') failed: %v", dec.Err())
		}
		// readBytes is now 3 > MaxSize (2) with dec.Err() == nil.
		if dec.NextBytesFold(" (LAZY") {
			t.Fatalf("NextBytesFold succeeded when readBytes > MaxSize; Err()=%v", dec.Err())
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want %v", dec.Err(), ErrMaxSize)
		}
	})

	t.Run("multi-byte Peek does not read past MaxSize or clobber ErrMaxSize", func(t *testing.T) {
		const limit = 10
		sentinel := errors.New("read past MaxSize budget")
		src := &prefixThenErrReader{
			prefix: strings.Repeat("A", limit) + " ",
			err:    sentinel,
		}
		dec := NewDecoder(bufio.NewReader(src), ConnSideServer)
		dec.MaxSize = limit

		var atom string
		if !dec.ExpectAtom(&atom) {
			t.Fatalf("ExpectAtom failed: %v", dec.Err())
		}
		if src.reads != 1 {
			t.Fatalf("setup reads = %d, want 1", src.reads)
		}

		// Byte 11 (' ') is already buffered and matches s[0]; byte 12 ('(') is
		// beyond MaxSize and must fail with ErrMaxSize without reading from src.
		if dec.NextBytesFold(" (LAZY") {
			t.Fatal("NextBytesFold returned true past MaxSize")
		}
		if src.reads != 1 {
			t.Fatalf("NextBytesFold performed %d underlying reads (read past MaxSize), want 1; Err()=%v",
				src.reads, dec.Err())
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want %v", dec.Err(), ErrMaxSize)
		}
	})

	t.Run("truncated match past MaxSize reports ErrMaxSize across DiscardLine", func(t *testing.T) {
		const limit = 10
		raw := strings.Repeat("A", limit) + " (LAZ"
		dec := NewDecoder(bufio.NewReader(strings.NewReader(raw)), ConnSideServer)
		dec.MaxSize = limit

		var atom string
		if !dec.ExpectAtom(&atom) {
			t.Fatalf("ExpectAtom failed: %v", dec.Err())
		}
		if dec.NextBytesFold(" (LAZY") {
			t.Fatal("NextBytesFold succeeded on truncated input")
		}
		dec.DiscardLine()
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() after NextBytesFold+DiscardLine = %v, want %v", dec.Err(), ErrMaxSize)
		}
	})

	t.Run("promotes ErrMaxSize while discarding", func(t *testing.T) {
		dec := NewDecoder(bufio.NewReader(strings.NewReader(" (")), ConnSideServer)
		dec.MaxSize = 1
		dec.readBytes = 2
		dec.err = &DecoderExpectError{Message: "prior syntax error"}
		dec.discarding = true

		if dec.NextBytesFold(" (") {
			t.Fatal("NextBytesFold succeeded past MaxSize while discarding")
		}
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want %v", dec.Err(), ErrMaxSize)
		}
	})
}

func TestDecoderDiscardLinePeekPastMaxSizeAndOpenLiteral(t *testing.T) {
	t.Run("does not read from transport past MaxSize after Text hits ErrMaxSize", func(t *testing.T) {
		const limit = 10
		sentinel := errors.New("read past MaxSize budget")
		src := &prefixThenErrReader{
			prefix: " " + strings.Repeat("A", limit),
			err:    sentinel,
		}
		dec := NewDecoder(bufio.NewReader(src), ConnSideServer)
		dec.MaxSize = limit

		if dec.ExpectCRLF() {
			t.Fatal("ExpectCRLF unexpectedly succeeded")
		}
		if src.reads != 1 {
			t.Fatalf("setup reads = %d, want 1", src.reads)
		}

		dec.DiscardLine()
		if !errors.Is(dec.Err(), ErrMaxSize) {
			t.Fatalf("dec.Err() = %v, want %v", dec.Err(), ErrMaxSize)
		}
		if src.reads != 1 {
			t.Fatalf("DiscardLine performed %d underlying reads (read past MaxSize), want 1", src.reads)
		}
	})

	t.Run("does not block peeking synchronizing literal after literal-open error", func(t *testing.T) {
		r, w := io.Pipe()
		defer r.Close()
		defer w.Close()

		go func() {
			_, _ = w.Write([]byte("{5}\r\n"))
		}()

		dec := NewDecoder(bufio.NewReader(r), ConnSideServer)
		_, nonSync, ok := dec.LiteralReader()
		if !ok || nonSync {
			t.Fatalf("LiteralReader = (_, %v, %v), err=%v; want sync literal", nonSync, ok, dec.Err())
		}
		// Trigger an invalid decode while the synchronizing literal is open.
		_ = dec.ExpectCRLF()

		done := make(chan struct{})
		go func() {
			dec.DiscardLine()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			_ = w.Close()
			<-done
			t.Fatal("DiscardLine blocked peeking unread synchronizing literal octets")
		}
	})
}

func TestDecoderAllowStrayQuotesMaxSizeAndReadError(t *testing.T) {
	t.Run("records non-EOF transport error on stray quote peek", func(t *testing.T) {
		injectedErr := errors.New("injected transport timeout")
		src := &chunksThenErrReader{
			chunks: []string{`"hello"`, " world\r\n"},
			errIdx: 1,
			err:    injectedErr,
		}
		dec := NewDecoder(bufio.NewReader(src), ConnSideServer)

		var s string
		ok := dec.ExpectStringAllowStrayQuotes(&s)
		if ok || !errors.Is(dec.Err(), injectedErr) {
			t.Fatalf("ExpectStringAllowStrayQuotes = %v, Err() = %v; want false with %v",
				ok, dec.Err(), injectedErr)
		}
		if !dec.readFailed {
			t.Fatal("expected dec.readFailed = true after non-EOF transport error in stray-quote peek")
		}
		dec.DiscardLine()
		if !dec.Desynchronized() {
			t.Fatal("expected server decoder to mark stream desynchronized after transport read failure")
		}
		if src.calls != 2 {
			t.Fatalf("performed %d reads (read again after transport error), want 2", src.calls)
		}
	})

	t.Run("does not read from transport past MaxSize on conformant quoted string", func(t *testing.T) {
		const limit = 5
		sentinel := errors.New("read past MaxSize budget")
		src := &prefixThenErrReader{
			prefix: `"AAAA"`, // 6 bytes == limit + 1
			err:    sentinel,
		}
		dec := NewDecoder(bufio.NewReader(src), ConnSideClient)
		dec.MaxSize = limit

		var s string
		if !dec.ExpectStringAllowStrayQuotes(&s) {
			t.Fatalf("ExpectStringAllowStrayQuotes failed: %v", dec.Err())
		}
		if src.reads != 1 {
			t.Fatalf("ExpectStringAllowStrayQuotes performed %d underlying reads (read past MaxSize), want 1",
				src.reads)
		}
	})

	t.Run("does not consume stray quotes beyond MaxSize", func(t *testing.T) {
		const limit = 5
		dec := NewDecoder(bufio.NewReader(strings.NewReader(`"AAAA"""`)), ConnSideClient)
		dec.MaxSize = limit

		var s string
		if dec.ExpectStringAllowStrayQuotes(&s) {
			syntax, _ := dec.ReadCount()
			if syntax > limit+1 {
				t.Fatalf("ExpectStringAllowStrayQuotes consumed %d bytes with MaxSize=%d and Err()=%v",
					syntax, limit, dec.Err())
			}
		}
	})
}

func TestDecoderEOFHonorsErrorAndLiteralGuards(t *testing.T) {
	t.Run("stops after recorded error without second read or false clean EOF", func(t *testing.T) {
		injectedErr := errors.New("injected transport read failure")
		src := &chunksThenErrReader{
			chunks: nil,
			errIdx: 0,
			err:    injectedErr,
		}
		dec := NewDecoder(bufio.NewReader(src), ConnSideServer)
		if dec.EOF() {
			t.Fatal("first EOF() returned true on transport error")
		}
		if !errors.Is(dec.Err(), injectedErr) {
			t.Fatalf("dec.Err() = %v, want %v", dec.Err(), injectedErr)
		}
		if dec.EOF() {
			t.Fatal("second EOF() returned true (clean EOF) after prior transport read error")
		}
		if src.calls != 1 {
			t.Fatalf("second EOF() performed %d underlying reads after recorded error, want 1", src.calls)
		}
	})

	t.Run("rejects EOF while literal is open", func(t *testing.T) {
		dec := NewDecoder(bufio.NewReader(strings.NewReader("{0+}\r\n")), ConnSideServer)
		_, _, ok := dec.LiteralReader()
		if !ok {
			t.Fatalf("LiteralReader failed: %v", dec.Err())
		}
		if dec.EOF() {
			t.Fatal("EOF() returned true while literal was still open")
		}
		if dec.Err() == nil {
			t.Fatal("expected EOF() while literal is open to record decoder error")
		}
	})
}
