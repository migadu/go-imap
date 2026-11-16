package imapserver_test

import (
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func TestOverlongCommandDoesNotStallOrMisclassifyAtMaxSize(t *testing.T) {
	const serverMaxCommandSize = 50 * 1024

	t.Run("DiscardLine answers immediately at maxCommandSize+1 without waiting for more input", func(t *testing.T) {
		var logged syncTLSHookLog
		wc := dialWire(t, newAuthenticateServer(t, &logged))

		// Exactly maxCommandSize + 1 bytes without CRLF while keeping the write
		// side open: once DiscardLine's Text read hits ErrMaxSize, the server
		// must immediately answer with BAD + BYE rather than blocking in an
		// extra Peek(1) until cmdReadTimeout (30s) expires.
		prefix := "a1 NOOP "
		payload := prefix + strings.Repeat("A", serverMaxCommandSize+1-len(prefix))
		if _, err := io.WriteString(wc.conn, payload); err != nil {
			t.Fatalf("WriteString: %v", err)
		}

		wc.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		line1, err := wc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading tagged BAD stalled or failed: %v (partial %q)", err, line1)
		}
		if !strings.HasPrefix(line1, "a1 BAD [CLIENTBUG] Command line too long") {
			t.Fatalf("line1 = %q, want a1 BAD [CLIENTBUG] Command line too long", line1)
		}
		line2, err := wc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading closing BYE failed: %v (partial %q)", err, line2)
		}
		if !strings.HasPrefix(line2, "* BYE Command too long") {
			t.Fatalf("line2 = %q, want * BYE Command too long", line2)
		}
	})

	t.Run("PREVIEW lookahead past maxCommandSize reports Command line too long rather than stream desync", func(t *testing.T) {
		mem := imapmemserver.New()
		mem.AddUser(newTestUser(t))
		var logged syncTLSHookLog
		server := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return mem.NewSession(), nil, nil
			},
			InsecureAuth: true,
			Caps: imap.CapSet{
				imap.CapIMAP4rev1: {},
				imap.CapPreview:   {},
			},
			Logger: log.New(&logged, "", 0),
		})
		ln, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatalf("net.Listen() = %v", err)
		}
		go server.Serve(ln)
		t.Cleanup(func() { server.Close() })

		wc := dialWire(t, ln.Addr().String())
		for _, cmd := range []string{`LOGIN "user" "pass"`, `SELECT "INBOX"`} {
			tag := wc.send(t, cmd)
			lines, err := wc.waitTagged(tag)
			if err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
			if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK") {
				t.Fatalf("%s = %q, want %s OK", cmd, last, tag)
			}
		}

		// Build a command whose syntax reaches exactly serverMaxCommandSize at
		// the end of "PREVIEW", followed by a truncated " (LAZ" past the cap.
		head := "a9 FETCH 1 ("
		tail := "PREVIEW"
		var b strings.Builder
		b.Grow(serverMaxCommandSize + 8)
		b.WriteString(head)
		need := serverMaxCommandSize - len(head) - len(tail)
		for need >= 6 {
			b.WriteString("FLAGS ")
			need -= 6
		}
		if need > 0 {
			// Replace the last "FLAGS " with a padded section/atom or leading
			// spaces? Server lists reject extra spaces, so adjust tag length to
			// hit serverMaxCommandSize modulo 6 cleanly.
			b.Reset()
			tagPad := strings.Repeat("9", need)
			b.WriteString("a9" + tagPad + " FETCH 1 (")
			rem := serverMaxCommandSize - b.Len() - len(tail)
			for rem > 0 {
				b.WriteString("FLAGS ")
				rem -= 6
			}
		}
		b.WriteString(tail)
		if b.Len() != serverMaxCommandSize {
			t.Fatalf("constructed prefix length = %d, want %d", b.Len(), serverMaxCommandSize)
		}
		b.WriteString(" (LAZ")

		if _, err := io.WriteString(wc.conn, b.String()); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		if cw, ok := wc.conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}

		wc.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		all, err := io.ReadAll(wc.br)
		if err != nil {
			t.Fatalf("ReadAll: %v (got %q)", err, string(all))
		}
		resp := string(all)
		if !strings.Contains(resp, "BAD [CLIENTBUG] Command line too long\r\n") {
			t.Fatalf("response missing BAD [CLIENTBUG] Command line too long:\n%s", resp)
		}
		if !strings.Contains(resp, "* BYE Command too long\r\n") {
			t.Fatalf("response missing * BYE Command too long:\n%s", resp)
		}
	})
}
