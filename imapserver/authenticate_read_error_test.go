package imapserver_test

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A client that stops in the middle of an AUTHENTICATE exchange, after the
// continuation request, has not caused a server failure, and the octets it sent
// before stopping are not a response: "The client response consists of a single
// line" (RFC 9051 §6.2.2), and a line ends in CRLF.
//
// A closed connection ends the connection without a word, as a read error
// between commands does. A read timeout is an inactivity autologout, announced
// with an untagged BYE (RFC 9051 §7.1.5) before the close, as for IDLE. Neither
// is answered with a tagged response, nor logged, and a partial line -- here one
// carrying valid credentials -- is never authenticated against.
func TestAuthenticateResponseReadFailureEndsConnection(t *testing.T) {
	validPlain := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	overlong := strings.Repeat("A", 5000)
	for _, tc := range []struct {
		name    string
		partial string // sent after the continuation request, with no line ending
		timeout bool   // go silent until the read deadline instead of closing
	}{
		{name: "closed", partial: ""},
		{name: "closed after partial line", partial: validPlain},
		{name: "closed inside over-long line", partial: overlong},
		{name: "timeout", partial: "", timeout: true},
		{name: "timeout after partial line", partial: validPlain, timeout: true},
		{name: "timeout inside over-long line", partial: overlong, timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged syncTLSHookLog
			var wrap func(net.Listener) net.Listener
			if tc.timeout {
				wrap = func(ln net.Listener) net.Listener {
					return idleTimeoutListener{Listener: ln, idle: 300 * time.Millisecond}
				}
			}
			wc := dialWire(t, newAuthenticateServerWithListener(t, &logged, wrap))

			tag := wc.send(t, "AUTHENTICATE PLAIN")
			if line, err := wc.readLine(); err != nil || line != "+ " {
				t.Fatalf("continuation request = %q, %v", line, err)
			}
			if _, err := io.WriteString(wc.conn, tc.partial); err != nil {
				t.Fatal(err)
			}
			if !tc.timeout {
				if err := wc.conn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
			}

			var lines []string
			for {
				line, err := wc.readLine()
				if err != nil {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						t.Fatalf("connection stayed open after the failed read; read %q", lines)
					}
					break
				}
				lines = append(lines, line)
			}

			for _, line := range lines {
				if strings.HasPrefix(line, tag+" ") {
					t.Errorf("AUTHENTICATE was answered with %q after its response could not be read", line)
				}
			}
			if tc.timeout {
				if len(lines) != 1 || !strings.HasPrefix(lines[0], "* BYE ") {
					t.Errorf("after the read timeout got %q, want one untagged BYE", lines)
				}
			} else if len(lines) != 0 {
				t.Errorf("after the client closed got %q, want nothing", lines)
			}
			if out := logged.String(); out != "" {
				t.Errorf("client read failure logged as a server failure: %q", out)
			}
		})
	}
}
