package imapwire

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

// errAfterReader yields data, then fails every subsequent read with err.
type errAfterReader struct {
	data string
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// TestLiteralReaderTruncated is a regression test for a short literal being
// reported as a complete one.
//
// A peer announces {N} and then goes away after fewer than N bytes. The literal
// was wrapped in io.LimitReader, which reports the underlying io.EOF verbatim,
// so the reader looked like it had delivered a complete value: io.ReadAll
// returned the short payload with a nil error and the caller had no way to tell
// it apart from a full read.
//
// That is silent data truncation in both directions. A client hands a truncated
// message body to the application as if it were whole; a server accepts a
// truncated APPEND literal and stores it as a complete message.
//
// Upstream report: emersion/go-imap#650, fix proposed in emersion/go-imap#676
// by vzeroupper.
func TestLiteralReaderTruncated(t *testing.T) {
	dec := NewDecoder(bufio.NewReader(strings.NewReader("{10}\r\nabc")), ConnSideClient)

	lit, _, ok := dec.LiteralReader()
	if !ok {
		t.Fatalf("LiteralReader() = %v", dec.Err())
	}
	if lit.Size() != 10 {
		t.Fatalf("Size() = %v, want 10", lit.Size())
	}

	b, err := io.ReadAll(lit)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("io.ReadAll(lit) = %q, %v; want io.ErrUnexpectedEOF (literal truncated at %d of %d bytes)", b, err, len(b), lit.Size())
	}
}

// TestLiteralReaderComplete pins the case that must not change: a literal that
// delivers exactly the announced number of bytes still ends in a clean io.EOF.
func TestLiteralReaderComplete(t *testing.T) {
	dec := NewDecoder(bufio.NewReader(strings.NewReader("{3}\r\nabcRest\r\n")), ConnSideClient)

	lit, _, ok := dec.LiteralReader()
	if !ok {
		t.Fatalf("LiteralReader() = %v", dec.Err())
	}

	b, err := io.ReadAll(lit)
	if err != nil {
		t.Fatalf("io.ReadAll(lit) = %v, want nil", err)
	}
	if string(b) != "abc" {
		t.Fatalf("io.ReadAll(lit) = %q, want %q", b, "abc")
	}
	if dec.Err() != nil {
		t.Fatalf("Err() = %v, want nil", dec.Err())
	}
	// The literal must be released so the decoder can carry on with the
	// response, and it must not have eaten the bytes that follow it.
	if dec.literal {
		t.Error("decoder still has a literal open after a complete read")
	}
	var atom string
	if !dec.Atom(&atom) || atom != "Rest" {
		t.Errorf("Atom() = %q, %v; want %q", atom, dec.Err(), "Rest")
	}
}

// TestLiteralReaderNonEOFError is a regression test for a non-EOF read failure
// leaving the decoder wedged.
//
// LiteralReader.Read released the literal only on io.EOF, so any other failure
// (an i/o timeout being the common one) left dec.literal set and dec.err unset.
// Every later decode then failed with "cannot decode while a literal is open" —
// a message about our own state — instead of the i/o error that actually broke
// the connection.
func TestLiteralReaderNonEOFError(t *testing.T) {
	errBoom := errors.New("boom")
	dec := NewDecoder(bufio.NewReader(&errAfterReader{data: "{10}\r\nabc", err: errBoom}), ConnSideClient)

	lit, _, ok := dec.LiteralReader()
	if !ok {
		t.Fatalf("LiteralReader() = %v", dec.Err())
	}

	if _, err := io.ReadAll(lit); !errors.Is(err, errBoom) {
		t.Fatalf("io.ReadAll(lit) = %v, want %v", err, errBoom)
	}

	if dec.literal {
		t.Error("decoder still has a literal open after a failed read")
	}
	if !errors.Is(dec.Err(), errBoom) {
		t.Errorf("Err() = %v, want %v", dec.Err(), errBoom)
	}

	// The wedged decoder reported its own state instead of the real cause.
	var atom string
	dec.Atom(&atom)
	if err := dec.Err(); err != nil && strings.Contains(err.Error(), "cannot decode while a literal is open") {
		t.Errorf("Err() = %v, want the underlying i/o error", err)
	}
}

func TestLiteralReaderTerminalError(t *testing.T) {
	wrappedEOF := fmt.Errorf("read failed: %w", io.EOF)
	tests := []struct {
		name    string
		input   string
		readErr error
		body    string
		wantErr error
	}{
		{name: "complete_literal", input: "{5}\r\nhello)", body: "hello"},
		{name: "empty_literal", input: "{0}\r\n)", body: ""},
		{name: "quoted_string", input: "\"hello\")", body: "hello"},
		{name: "short_literal", input: "{8}\r\nshort", body: "short", wantErr: io.ErrUnexpectedEOF},
		{name: "missing_literal", input: "{8}\r\n", wantErr: io.ErrUnexpectedEOF},
		{name: "read_timeout", input: "{8}\r\nshort", readErr: os.ErrDeadlineExceeded, body: "short", wantErr: os.ErrDeadlineExceeded},
		{name: "closed_pipe", input: "{8}\r\nshort", readErr: io.ErrClosedPipe, body: "short", wantErr: io.ErrClosedPipe},
		{name: "wrapped_eof", input: "{8}\r\nshort", readErr: wrappedEOF, body: "short", wantErr: wrappedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var source io.Reader = strings.NewReader(tt.input)
			if tt.readErr != nil {
				source = io.MultiReader(source, iotest.ErrReader(tt.readErr))
			}
			dec := NewDecoder(bufio.NewReader(source), ConnSideClient)
			lit, _, ok := dec.ExpectNStringReader()
			if !ok || lit == nil {
				t.Fatalf("ExpectNStringReader() = %v, %v; decoder error: %v", lit, ok, dec.Err())
			}
			counter := &literalReadCounter{Reader: lit.r}
			lit.r = counter
			body, err := io.ReadAll(lit)
			if string(body) != tt.body || !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadAll() = %q, %v; want %q, %v", body, err, tt.body, tt.wantErr)
			}
			if !errors.Is(dec.Err(), tt.wantErr) {
				t.Errorf("decoder error = %v; want %v", dec.Err(), tt.wantErr)
			}
			if dec.literal {
				t.Error("literal remains open after its terminal read")
			}
			calls := counter.calls
			wantTerminalErr := tt.wantErr
			if wantTerminalErr == nil {
				wantTerminalErr = io.EOF
			}
			for i := 0; i < 2; i++ {
				n, err := lit.Read(make([]byte, 1))
				if n != 0 || !errors.Is(err, wantTerminalErr) {
					t.Fatalf("Read() after completion = %v, %v; want 0, %v", n, err, wantTerminalErr)
				}
			}
			if counter.calls != calls {
				t.Errorf("underlying reader called after completion: %v -> %v", calls, counter.calls)
			}
			if tt.wantErr == nil && !dec.ExpectSpecial(')') {
				t.Errorf("cannot decode the next field: %v", dec.Err())
			}
		})
	}
}

type literalReadCounter struct {
	io.Reader
	calls int
}

func (r *literalReadCounter) Read(p []byte) (int, error) {
	r.calls++
	return r.Reader.Read(p)
}
