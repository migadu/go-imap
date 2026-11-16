package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// metadataLimitSession accepts SETMETADATA values up to maxSize octets and
// counts the commands that reach it.
type metadataLimitSession struct {
	imapserver.Session
	maxSize uint32
	calls   *atomic.Int32
}

func (s *metadataLimitSession) GetMetadata(_ context.Context, mailbox string, _ []string, _ *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	return &imap.GetMetadataData{Mailbox: mailbox, Entries: map[string]*[]byte{}}, nil
}

func (s *metadataLimitSession) SetMetadata(context.Context, string, map[string]*[]byte) error {
	s.calls.Add(1)
	return nil
}

func (s *metadataLimitSession) MetadataMaxSize() uint32 { return s.maxSize }

func dialMetadataLimitServer(t *testing.T, maxSize uint32) (*parseWire, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	w := dialParseServer(t, func(s imapserver.Session) imapserver.Session {
		return &metadataLimitSession{Session: s, maxSize: maxSize, calls: calls}
	})
	return w, calls
}

// A value literal over the session's limit is refused by its announced size,
// before the client sends it, with the code RFC 5464 §4.3 requires: "the server
// MUST return a tagged NO response with a "[METADATA MAXSIZE NNN]" response
// code when NNN is the maximum octet count that it is willing to accept."
func TestSetMetadataLiteralOverSessionMaxSize(t *testing.T) {
	w, calls := dialMetadataLimitServer(t, 1024)

	fmt.Fprint(w.conn, "a2 SETMETADATA \"\" (/private/comment {1025}\r\n")
	if got := w.line(); got != "a2 NO [METADATA MAXSIZE 1024] Metadata values are limited to 1024 bytes" {
		t.Fatalf("got %q; want NO [METADATA MAXSIZE 1024] before any continuation request", got)
	}

	fmt.Fprint(w.conn, "a3 SETMETADATA \"\" (/private/comment {1024}\r\n")
	if got := w.line(); !strings.HasPrefix(got, "+") {
		t.Fatalf("got %q; want a continuation request for a value at the limit", got)
	}
	fmt.Fprint(w.conn, strings.Repeat("x", 1024)+")\r\n")
	if got := w.until("a3"); !strings.HasPrefix(got, "a3 OK") {
		t.Fatalf("got %q; want OK", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("SetMetadata called %d times; want 1", n)
	}
}

// The values of one SETMETADATA are held in memory together until the session
// sees them, and literal octets do not count against the command size limit.
// Each value within the session's limit must not let one command buffer an
// unbounded number of them: past the bound on their sum the next literal is
// refused with NO [LIMIT] before it is sent, and the session never sees the
// command.
func TestSetMetadataLiteralValuesBoundedInTotal(t *testing.T) {
	const maxSize = 64 << 10
	w, calls := dialMetadataLimitServer(t, maxSize)
	value := strings.Repeat("x", maxSize)

	fmt.Fprint(w.conn, "a2 SETMETADATA \"\" (")
	for i := 0; ; i++ {
		if i > 0 {
			fmt.Fprint(w.conn, " ")
		}
		fmt.Fprintf(w.conn, "/private/e%d {%d}\r\n", i, maxSize)
		got := w.line()
		if strings.HasPrefix(got, "+") {
			if i*maxSize > 64<<20 {
				t.Fatalf("server accepted %d values of %d octets (%d MiB) in one command", i+1, maxSize, (i+1)*maxSize>>20)
			}
			fmt.Fprint(w.conn, value)
			continue
		}
		if !strings.HasPrefix(got, "a2 NO [LIMIT] ") {
			t.Fatalf("value %d: got %q; want NO [LIMIT]", i, got)
		}
		if total := i * maxSize; total != 4<<20 {
			t.Errorf("refused after %d octets of values; want %d", total, 4<<20)
		}
		break
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("SetMetadata called %d times; want 0", n)
	}

	fmt.Fprint(w.conn, "a3 NOOP\r\n")
	if got := w.until("a3"); !strings.HasPrefix(got, "a3 OK") {
		t.Errorf("NOOP after the refused SETMETADATA = %q; want OK", got)
	}
}

// A value as large as the session accepts is never refused by the bound on the
// sum, even when that is above the bound.
func TestSetMetadataLiteralValueAboveTotalBound(t *testing.T) {
	const maxSize = 5 << 20
	w, calls := dialMetadataLimitServer(t, maxSize)

	fmt.Fprintf(w.conn, "a2 SETMETADATA \"\" (/private/comment {%d}\r\n", maxSize)
	if got := w.line(); !strings.HasPrefix(got, "+") {
		t.Fatalf("got %q; want a continuation request for a value at the session's limit", got)
	}
	fmt.Fprint(w.conn, strings.Repeat("x", maxSize)+")\r\n")
	if got := w.until("a2"); !strings.HasPrefix(got, "a2 OK") {
		t.Fatalf("got %q; want OK", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("SetMetadata called %d times; want 1", n)
	}
}
