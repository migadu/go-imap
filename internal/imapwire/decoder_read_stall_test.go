package imapwire

import (
	"bufio"
	"io"
	"os"
	"testing"
)

// stallReader yields its chunks one Read at a time and fails with a timeout
// between them, as a connection does when a read deadline expires in the
// middle of a line and the client sends the rest afterwards.
type stallReader struct {
	chunks []string
	stall  bool
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.stall {
		r.stall = false
		return 0, os.ErrDeadlineExceeded
	}
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
		r.stall = len(r.chunks) > 0
	}
	return n, nil
}

// A read that fails before the end of the line leaves the rest of the command
// unread: whether the line ends in a non-synchronizing literal announcement
// (RFC 7888 §3) is not known, and its octets may follow. The server has to
// close the connection instead of reading what arrives later as a command.
func TestServerReadFailureInsideLineStopsRecovery(t *testing.T) {
	const payload = "x1 NOOP\r\n"
	for _, tc := range []struct {
		name    string
		first   string
		rest    string
		literal bool
	}{
		{"after open brace", "{", "9+}\r\n" + payload, true},
		{"inside digits", "{9", "+}\r\n" + payload, true},
		{"before close brace", "{9+", "}\r\n" + payload, true},
		{"after close brace", "{9+}", "\r\n" + payload, true},
		{"after CR", "{9+}\r", "\n" + payload, true},
		{"unparsed before close brace", " {9+", "}\r\n" + payload, false},
		{"unparsed after close brace", " {9+}", "\r\n" + payload, false},
		{"unparsed text", " foo", " {9+}\r\n" + payload, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(&stallReader{chunks: []string{tc.first, tc.rest}})
			dec := NewDecoder(r, ConnSideServer)
			if tc.literal {
				if _, _, ok := dec.LiteralReader(); ok {
					t.Fatalf("LiteralReader(%q) accepted a header cut off by a read failure", tc.first)
				}
			} else if dec.ExpectCRLF() {
				t.Fatalf("ExpectCRLF(%q) succeeded", tc.first)
			}
			dec.DiscardLine()
			if !dec.Desynchronized() {
				t.Fatalf("read failure after %q left the stream in service (err: %v)", tc.first, dec.Err())
			}
			dec.DiscardLine()
			if rest, _ := io.ReadAll(r); string(rest) != tc.rest {
				t.Fatalf("recovery consumed input after the read failure: %q left, want %q", rest, tc.rest)
			}
		})
	}

	// The client side keeps its behaviour: it has no command stream to protect.
	t.Run("client side", func(t *testing.T) {
		r := bufio.NewReader(&stallReader{chunks: []string{" foo", " bar\r\n"}})
		dec := NewDecoder(r, ConnSideClient)
		dec.ExpectCRLF()
		dec.DiscardLine()
		if dec.Desynchronized() {
			t.Fatal("client decoder marked desynchronized")
		}
	})
}
