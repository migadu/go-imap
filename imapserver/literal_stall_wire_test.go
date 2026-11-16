package imapserver_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// A client that stalls past the command read timeout inside a line, and then
// sends the rest, must not have what follows read as commands: the line may end
// in a non-synchronizing literal announcement (RFC 7888 §3) whose octets are
// client data. The read failure leaves the stream away from a command boundary,
// so the server ends the connection (RFC 9051 §2.2.1).
func TestReadTimeoutInsideCommandLineClosesConnection(t *testing.T) {
	// 30s cmdReadTimeout -> 500ms.
	const stall = 30 * time.Second / saslStepDeadlineScale * 3 / 2

	for _, tc := range []struct {
		name  string
		first string
		rest  string
	}{
		{"literal after open brace", "a1 LOGIN {", "9+}\r\nx1 NOOP\r\n pass1\r\n"},
		{"literal inside digits", "a1 LOGIN {9", "+}\r\nx1 NOOP\r\n pass1\r\n"},
		{"literal before close brace", "a1 LOGIN {9+", "}\r\nx1 NOOP\r\n pass1\r\n"},
		{"literal after close brace", "a1 LOGIN {9+}", "\r\nx1 NOOP\r\n pass1\r\n"},
		{"literal after CR", "a1 LOGIN {9+}\r", "\nx1 NOOP\r\n pass1\r\n"},
		{"unparsed literal before close brace", "a1 NOOP {9+", "}\r\nx1 NOOP\r\n"},
		{"unparsed literal after close brace", "a1 NOOP {9+}", "\r\nx1 NOOP\r\n"},
		{"unparsed text", "a1 NOOP foo", " {9+}\r\nx1 NOOP\r\n"},
		{"before line ending", "a1 NOOP ", "{9+}\r\nx1 NOOP\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mem := imapmemserver.New()
			u := imapmemserver.NewUser("user1", "pass1")
			if err := u.Create(context.Background(), "INBOX", nil); err != nil {
				t.Fatal(err)
			}
			mem.AddUser(u)
			srv := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return mem.NewSession(), nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapLiteralPlus: {}},
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ln.Close()
				srv.Close()
			})
			go srv.Serve(scaledBothDeadlinesListener{ln}) //nolint:errcheck

			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })

			if _, err := io.WriteString(c, tc.first); err != nil {
				t.Fatal(err)
			}
			time.Sleep(stall)
			// The server may already have closed the connection.
			_, _ = io.WriteString(c, tc.rest)

			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			out, err := io.ReadAll(c)
			transcript := string(out)
			if strings.Contains(transcript, "x1 ") {
				t.Fatalf("octets after the stall were read as a command:\n%s", transcript)
			}
			// The server closes with input unread, so the close may arrive as a
			// reset instead of EOF. Only a connection still open is a failure.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("connection stayed in service after a read timeout inside a command line:\n%s", transcript)
			}
		})
	}
}
