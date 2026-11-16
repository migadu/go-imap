package imapserver_test

import (
	"encoding/base64"
	"errors"
	"fmt"
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

// newAuthenticateServer starts a server whose sessions take AUTHENTICATE PLAIN
// over plaintext, logging to logged.
func newAuthenticateServer(t *testing.T, logged *syncTLSHookLog) string {
	t.Helper()
	return newAuthenticateServerWithListener(t, logged, nil)
}

func newAuthenticateServerWithListener(t *testing.T, logged *syncTLSHookLog, wrap func(net.Listener) net.Listener) string {
	t.Helper()
	mem := imapmemserver.New()
	mem.AddUser(newTestUser(t))
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		Logger:       log.New(logged, "", 0),
	})
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	if wrap != nil {
		ln = wrap(ln)
	}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return ln.Addr().String()
}

// An empty server challenge is an empty base64 string (RFC 4648 §10:
// BASE64("") = ""), so the continuation request is "+" SP and nothing else
// (RFC 9051 §6.2.2 and its AUTHENTICATE PLAIN example). "=" is the encoding
// of a zero-length initial response only, and is not base64 at all.
func TestAuthenticateEmptyChallengeIsPlusSpace(t *testing.T) {
	var logged syncTLSHookLog
	wc := dialWire(t, newAuthenticateServer(t, &logged))

	tag := wc.send(t, "AUTHENTICATE PLAIN")
	wc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := wc.br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading continuation request: %v (partial %q)", err, line)
	}
	if line != "+ \r\n" {
		t.Fatalf("continuation request for an empty challenge = %q, want %q", line, "+ \r\n")
	}

	resp := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	if _, err := io.WriteString(wc.conn, resp+"\r\n"); err != nil {
		t.Fatal(err)
	}
	lines, err := wc.waitTagged(tag)
	if err != nil {
		t.Fatal(err)
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK") {
		t.Fatalf("AUTHENTICATE PLAIN = %q, want %s OK", last, tag)
	}
}

// A client response line longer than the server will buffer is the client's
// doing: the exchange is rejected with a tagged BAD (RFC 9051 §6.2.2), not
// reported as a server failure, and the rest of the line is skipped so the
// connection stays usable.
func TestAuthenticateOverlongResponseIsBad(t *testing.T) {
	for _, size := range []int{5000, 20000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var logged syncTLSHookLog
			wc := dialWire(t, newAuthenticateServer(t, &logged))

			tag := wc.send(t, "AUTHENTICATE PLAIN")
			if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "+") {
				t.Fatalf("continuation request = %q, %v", line, err)
			}
			if _, err := io.WriteString(wc.conn, strings.Repeat("A", size)+"\r\n"); err != nil {
				t.Fatal(err)
			}
			lines, err := wc.waitTagged(tag)
			if err != nil {
				t.Fatal(err)
			}
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, tag+" BAD") || strings.Contains(last, "SERVERBUG") {
				t.Fatalf("over-long SASL response = %q, want %s BAD without SERVERBUG", last, tag)
			}
			if out := logged.String(); out != "" {
				t.Errorf("over-long SASL response logged as a server failure: %q", out)
			}

			// The rest of the over-long line was skipped, not parsed as a command.
			wc.login(t)
		})
	}
}

// ReadLine hides an EOF or timeout when it returns a nonempty final fragment.
// Neither that fragment nor a failed read without a fragment establishes a
// response-line boundary. The connection must end instead of interpreting any
// later bytes of the SASL response as a fresh command: after a timeout with an
// untagged BYE, after the client closed its side without a response, as for a
// close between commands (see TestAuthenticateResponseReadFailureEndsConnection).
func TestAuthenticateOverlongUnterminatedResponseCloses(t *testing.T) {
	for _, ending := range []string{"timeout", "EOF"} {
		for _, size := range []int{4096, 5000} {
			t.Run(fmt.Sprintf("%s/%d", ending, size), func(t *testing.T) {
				var wrap func(net.Listener) net.Listener
				if ending == "timeout" {
					wrap = func(ln net.Listener) net.Listener { return authenticateDeadlineListener{ln} }
				}
				var logged syncTLSHookLog
				wc := dialWire(t, newAuthenticateServerWithListener(t, &logged, wrap))
				wc.send(t, "AUTHENTICATE PLAIN")
				if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "+") {
					t.Fatalf("continuation request = %q, %v", line, err)
				}
				if _, err := io.WriteString(wc.conn, strings.Repeat("A", size)); err != nil {
					t.Fatal(err)
				}
				if ending == "EOF" {
					if err := wc.conn.(*net.TCPConn).CloseWrite(); err != nil {
						t.Fatal(err)
					}
				}
				if ending == "timeout" {
					if line, err := wc.readLine(); err != nil || !strings.HasPrefix(line, "* BYE ") {
						t.Fatalf("unterminated response = %q, %v, want BYE and connection close", line, err)
					}
				}
				if line, err := wc.readLine(); !errors.Is(err, io.EOF) {
					t.Fatalf("unterminated response = %q, %v, want EOF", line, err)
				}
			})
		}
	}
}

// Exercise the production timeout path without waiting for its 30-second
// command deadline. Existing shorter deadlines remain intact.
type authenticateDeadlineConn struct{ net.Conn }

func (c authenticateDeadlineConn) SetReadDeadline(deadline time.Time) error {
	limit := time.Now().Add(500 * time.Millisecond)
	if deadline.IsZero() || deadline.After(limit) {
		deadline = limit
	}
	return c.Conn.SetReadDeadline(deadline)
}

type authenticateDeadlineListener struct{ net.Listener }

func (ln authenticateDeadlineListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return authenticateDeadlineConn{conn}, nil
}
