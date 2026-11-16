package imapwire

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestLiteralLeavesStaleCRLFAcrossDiscardLine(t *testing.T) {
	t.Run("consumed literal followed by ExpectMailbox error discards remainder of line", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("{2+}\r\n&x hidden CREATE injected\r\nreal NOOP\r\n"))
		dec := NewDecoder(r, ConnSideServer)
		var mbox string
		if dec.ExpectMailbox(&mbox) {
			t.Fatal("ExpectMailbox accepted invalid UTF-7")
		}
		dec.DiscardLine()
		if dec.Desynchronized() {
			t.Fatal("complete line was marked desynchronized")
		}

		next := NewDecoder(r, ConnSideServer)
		var tag, cmd string
		if !next.ExpectAtom(&tag) || !next.ExpectSP() || !next.ExpectAtom(&cmd) || !next.ExpectCRLF() {
			t.Fatalf("next command parse failed: %v", next.Err())
		}
		if tag != "real" || cmd != "NOOP" {
			t.Fatalf("next command = %q %q, want \"real\" \"NOOP\" (DiscardLine left %q on wire)", tag, cmd, tag+" "+cmd)
		}
	})

	t.Run("consumed literal followed by unvalidated trailing nonsync literal desyncs", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("{2+}\r\n&x {24+}\r\nhidden CREATE injected\r\n"))
		dec := NewDecoder(r, ConnSideServer)
		var mbox string
		if dec.ExpectMailbox(&mbox) {
			t.Fatal("ExpectMailbox accepted invalid UTF-7")
		}
		dec.DiscardLine()
		if !dec.Desynchronized() {
			t.Fatal("DiscardLine missed trailing {24+} after consumed literal and left payload executable")
		}
	})

	t.Run("refused zero-length nonsync literal discards remainder of line", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("{0+}\r\n trailing\r\nreal NOOP\r\n"))
		dec := NewDecoder(r, ConnSideServer)
		dec.CheckBufferedLiteralFunc = func(size int64, nonSync bool) error {
			return errors.New("refused")
		}
		var s string
		if dec.Literal(&s) {
			t.Fatal("Literal succeeded despite CheckBufferedLiteralFunc error")
		}
		dec.DiscardLine()
		if dec.Desynchronized() {
			t.Fatal("complete line was marked desynchronized")
		}

		next := NewDecoder(r, ConnSideServer)
		var tag, cmd string
		if !next.ExpectAtom(&tag) || !next.ExpectSP() || !next.ExpectAtom(&cmd) || !next.ExpectCRLF() {
			t.Fatalf("next command parse failed: %v", next.Err())
		}
		if tag != "real" || cmd != "NOOP" {
			t.Fatalf("next command = %q %q, want \"real\" \"NOOP\"", tag, cmd)
		}
	})
}

func TestQuotedCrossesCRLFAndHidesNonSyncLiteral(t *testing.T) {
	t.Run("quoted string with nonsync literal and quote in payload", func(t *testing.T) {
		payload := "x\"y\r\nhidden CREATE injected\r\n"
		r := bufio.NewReader(strings.NewReader("\"foo {29+}\r\n" + payload))
		dec := NewDecoder(r, ConnSideServer)
		var s string
		if dec.Quoted(&s) {
			dec.ExpectCRLF()
			dec.DiscardLine()
			if !dec.Desynchronized() {
				next := NewDecoder(r, ConnSideServer)
				var tag, cmd string
				if next.ExpectAtom(&tag) && next.ExpectSP() && next.ExpectAtom(&cmd) {
					t.Fatalf("Quoted crossed CRLF and left literal payload executable as command %q %q", tag, cmd)
				}
			}
			t.Fatalf("Quoted accepted CR/LF inside quoted string: %q", s)
		}
		dec.DiscardLine()
		if !dec.Desynchronized() {
			t.Fatal("unclosed quoted string ending in {29+} was not marked desynchronized")
		}
	})

	t.Run("unclosed quoted string ending in nonsync literal stops before payload and desyncs", func(t *testing.T) {
		payload := "hidden CREATE injected\r\n"
		r := bufio.NewReader(strings.NewReader("\"foo {24+}\r\n" + payload))
		dec := NewDecoder(r, ConnSideServer)
		var s string
		if dec.Quoted(&s) {
			t.Fatalf("Quoted accepted unclosed string: %q", s)
		}
		dec.DiscardLine()
		if !dec.Desynchronized() {
			t.Fatal("unclosed quoted string ending in {24+} was not marked desynchronized")
		}
		if rest, _ := io.ReadAll(r); string(rest) != payload {
			t.Fatalf("Quoted/DiscardLine consumed literal payload: %q left, want %q", rest, payload)
		}
	})

	t.Run("quoted string rejects embedded CRLF and bare CR", func(t *testing.T) {
		for _, raw := range []string{"\"foo\r\nbar\"\r\n", "\"foo\rbar\"\r\n", "\"foo\\\r\nbar\"\r\n"} {
			dec := newServerDecoder(raw)
			var s string
			if dec.Quoted(&s) {
				t.Fatalf("Quoted(%q) accepted CR/LF: %q", raw, s)
			}
		}
	})
}

func TestExpectTextHidesTrailingNonSyncLiteral(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{24+}\r\nhidden CREATE injected\r\n"))
	dec := NewDecoder(r, ConnSideServer)
	var text string
	if !dec.ExpectText(&text) {
		t.Fatalf("ExpectText failed: %v", dec.Err())
	}
	dec.DiscardLine()
	if !dec.Desynchronized() {
		t.Fatal("DiscardLine missed {24+} consumed by ExpectText and left payload executable")
	}
}

func TestEOFReadFailureSetsReadFailed(t *testing.T) {
	r := bufio.NewReader(&stallReader{
		chunks: []string{"x1 NOOP\r\n"},
		stall:  true,
	})
	dec := NewDecoder(r, ConnSideServer)
	if dec.EOF() {
		t.Fatal("EOF() returned true on deadline exceeded")
	}
	if !errors.Is(dec.Err(), os.ErrDeadlineExceeded) {
		t.Fatalf("dec.Err() = %v, want os.ErrDeadlineExceeded", dec.Err())
	}
	dec.DiscardLine()
	if !dec.Desynchronized() {
		t.Fatal("DiscardLine after EOF() read failure left server stream in service")
	}
	if rest, _ := io.ReadAll(r); string(rest) != "x1 NOOP\r\n" {
		t.Fatalf("DiscardLine consumed bytes after EOF() read failure: got %q, want %q", rest, "x1 NOOP\r\n")
	}
}

func TestDiscardLinePreservesErrMaxSizeAfterExpectError(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(" extra" + strings.Repeat("A", 32) + "\r\n"))
	dec := NewDecoder(r, ConnSideServer)
	dec.MaxSize = 16
	if dec.ExpectCRLF() {
		t.Fatal("ExpectCRLF succeeded on trailing input")
	}
	dec.DiscardLine()
	if !errors.Is(dec.Err(), ErrMaxSize) {
		t.Fatalf("dec.Err() = %v, want ErrMaxSize", dec.Err())
	}
}
