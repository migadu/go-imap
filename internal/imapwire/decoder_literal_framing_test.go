package imapwire

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestServerLiteralRequiresImmediateCRLF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"sync CRLF", "{3}\r\n"},
		{"nonsync CRLF", "{3+}\r\n"},
		{"sync lone LF", "{3}\n"},
		{"nonsync lone LF", "{3+}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := newServerDecoder(tc.header + "abc\r\n")
			lit, _, ok := dec.LiteralReader()
			if !ok {
				t.Fatalf("LiteralReader(%q) failed: %v", tc.header, dec.Err())
			}
			got, err := io.ReadAll(lit)
			if err != nil || string(got) != "abc" {
				t.Fatalf("ReadAll(lit) = %q, %v; want %q, nil", got, err, "abc")
			}
			if !dec.ExpectCRLF() {
				t.Fatalf("ExpectCRLF after literal failed: %v", dec.Err())
			}
		})
	}

	// Client-side response decoding preserves the trailing-space workaround for
	// non-conformant servers (https://github.com/emersion/go-imap/issues/540).
	t.Run("client side tolerates trailing space", func(t *testing.T) {
		dec := NewDecoder(bufio.NewReader(strings.NewReader("{3} \r\nabc\r\n")), ConnSideClient)
		lit, _, ok := dec.LiteralReader()
		if !ok {
			t.Fatalf("client LiteralReader(\"{3} \\r\\n\") failed: %v", dec.Err())
		}
		got, err := io.ReadAll(lit)
		if err != nil || string(got) != "abc" {
			t.Fatalf("ReadAll(lit) = %q, %v; want %q, nil", got, err, "abc")
		}
	})

	// RFC 9051 §9 / RFC 7888 §3: on the server side, a line whose end before
	// CRLF is not "{" number64 ["+"] "}" does not announce a literal. The line
	// is a syntax error, and the next line is the next command.
	for _, header := range []string{
		"{3+} \r\n",
		"{3} \r\n",
		"{3+}  \r\n",
		"{3}  \r\n",
		"{3+}\t\r\n",
		"{3+} \n",
		"{3} \n",
		"{3+}x\r\n",
		"{3+x}\r\n",
		"{99999999999999999999999}x\r\n",
		"{99999999999x+}\r\n",
		"{-1+}\r\n",
		"{+3+}\r\n",
		"{x+}\r\n",
		"{3+}\r\r\n",
	} {
		t.Run(header, func(t *testing.T) {
			dec := newServerDecoder(header + "next NOOP\r\n")
			if _, _, ok := dec.LiteralReader(); ok {
				t.Fatalf("LiteralReader(%q) accepted non-literal header", header)
			}
			expectErrorRecorded(t, dec)
			dec.DiscardLine()
			if dec.Desynchronized() {
				t.Fatalf("header %q marked stream desynchronized; want recoverable syntax error", header)
			}
			next := NewDecoder(dec.r, ConnSideServer)
			var tag, cmd string
			if !next.ExpectAtom(&tag) || !next.ExpectSP() || !next.ExpectAtom(&cmd) || !next.ExpectCRLF() {
				t.Fatalf("next command after %q failed: %v", header, next.Err())
			}
			if tag != "next" || cmd != "NOOP" {
				t.Fatalf("next command = %q %q, want \"next\" \"NOOP\"", tag, cmd)
			}
		})
	}
}

func TestMalformedLiteralHeaderStopsRecovery(t *testing.T) {
	for _, header := range []string{
		"{9223372036854775808+}\r\n",
		"{99999999999+}\r\n",
		"{x} {3+}\r\n",
		"{{3+}\r\n",
		"{3+} {3+}\r\n",
		"{x} {99999999999999999999999+}\r\n",
	} {
		t.Run(header, func(t *testing.T) {
			dec := newServerDecoder(header + "abc\r\nnext NOOP\r\n")
			if _, _, ok := dec.LiteralReader(); ok {
				t.Fatal("malformed literal header was accepted")
			}
			expectErrorRecorded(t, dec)
			dec.DiscardLine()
			if !dec.Desynchronized() {
				t.Fatal("unvalidated trailing non-sync literal left payload eligible for command recovery")
			}
			remaining := dec.r.Buffered()
			dec.DiscardLine()
			if dec.r.Buffered() != remaining {
				t.Fatal("recovery consumed input after framing became uncertain")
			}
		})
	}

	t.Run("unparsed overflowed nonsync literal stops recovery", func(t *testing.T) {
		dec := newServerDecoder(" {99999999999999999999999+}\r\nabc\r\n")
		dec.ExpectCRLF()
		dec.DiscardLine()
		if !dec.Desynchronized() {
			t.Fatal("DiscardLine did not mark unparsed overflowed non-sync literal desynchronized")
		}
	})
}
