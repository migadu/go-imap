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

	"github.com/emersion/go-sasl"

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

// recordingSASLSession offers one mechanism that asks for a single response
// with an empty challenge and passes on whatever the server decoded for it.
type recordingSASLSession struct {
	imapserver.Session
	got chan []byte
}

func (s *recordingSASLSession) AuthenticateMechanisms() []string { return []string{"PLAIN"} }

func (s *recordingSASLSession) Authenticate(mech string) (sasl.Server, error) {
	return recordingSASLServer{s.got}, nil
}

type recordingSASLServer struct{ got chan<- []byte }

func (s recordingSASLServer) Next(resp []byte) ([]byte, bool, error) {
	if resp == nil {
		return []byte{}, false, nil
	}
	s.got <- resp
	return nil, true, nil
}

// "=" is a zero-length response only as the initial response
// (RFC 9051 §9: initial-resp = (base64 / "=")). A reply to a continuation
// request is base64 (authenticate = ... *(CRLF base64)), where a zero-length
// response is an empty line and a lone pad character is invalid base64, which
// the server MUST reject with a tagged BAD (RFC 9051 §6.2.2).
func TestAuthenticateLonePadOnlyInInitialResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmd     string
		replies []string // lines sent after each continuation request
		wantBad bool
	}{
		{name: "initial response", cmd: "AUTHENTICATE PLAIN ="},
		{name: "empty continuation reply", cmd: "AUTHENTICATE PLAIN", replies: []string{""}},
		{name: "pad as continuation reply", cmd: "AUTHENTICATE PLAIN", replies: []string{"="}, wantBad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan []byte, 1)
			mem := imapmemserver.New()
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &recordingSASLSession{Session: mem.NewSession(), got: got}, nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
				Logger:       log.New(io.Discard, "", 0),
			})
			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("net.Listen() = %v", err)
			}
			go server.Serve(ln)
			t.Cleanup(func() { server.Close() })
			wc := dialWire(t, ln.Addr().String())

			tag := wc.send(t, tc.cmd)
			for _, reply := range tc.replies {
				if line, err := wc.readLine(); err != nil || line != "+ " {
					t.Fatalf("continuation request = %q, %v, want %q", line, err, "+ ")
				}
				if _, err := io.WriteString(wc.conn, reply+"\r\n"); err != nil {
					t.Fatal(err)
				}
			}
			lines, err := wc.waitTagged(tag)
			if err != nil {
				t.Fatal(err)
			}
			last := lines[len(lines)-1]

			if tc.wantBad {
				if !strings.HasPrefix(last, tag+" BAD") {
					t.Fatalf("%s = %q, want %s BAD", tc.name, last, tag)
				}
				select {
				case resp := <-got:
					t.Fatalf("mechanism was given %q, want the reply rejected before it", resp)
				default:
				}
				// The command was rejected, not the connection.
				wc.mustOK(t, "NOOP")
				return
			}

			if !strings.HasPrefix(last, tag+" OK") {
				t.Fatalf("%s = %q, want %s OK", tc.name, last, tag)
			}
			select {
			case resp := <-got:
				if resp == nil || len(resp) != 0 {
					t.Fatalf("mechanism was given %#v, want a non-nil zero-length response", resp)
				}
			default:
				t.Fatal("mechanism was not given a response")
			}
		})
	}
}

// Go's base64.StdEncoding.DecodeString silently strips '\r' and '\n'. Because
// readSASLResponse removes only the single trailing CRLF, a bare CR earlier on
// a continuation line must be rejected with tagged BAD as outside the RFC 9051
// §9 base64 alphabet rather than stripped and passed to the SASL mechanism.
func TestAuthenticateContinuationRefusesBareCR(t *testing.T) {
	validB64 := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	for _, tc := range []struct {
		name  string
		reply string
	}{
		{name: "bare CR only", reply: "\r"},
		{name: "embedded bare CR", reply: validB64[:4] + "\r" + validB64[4:]},
		{name: "trailing bare CR", reply: validB64 + "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan []byte, 1)
			mem := imapmemserver.New()
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &recordingSASLSession{Session: mem.NewSession(), got: got}, nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
				Logger:       log.New(io.Discard, "", 0),
			})
			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("net.Listen() = %v", err)
			}
			go server.Serve(ln)
			t.Cleanup(func() { server.Close() })
			wc := dialWire(t, ln.Addr().String())

			tag := wc.send(t, "AUTHENTICATE PLAIN")
			if line, err := wc.readLine(); err != nil || line != "+ " {
				t.Fatalf("continuation request = %q, %v, want %q", line, err, "+ ")
			}
			if _, err := io.WriteString(wc.conn, tc.reply+"\r\n"); err != nil {
				t.Fatal(err)
			}
			lines, err := wc.waitTagged(tag)
			if err != nil {
				t.Fatal(err)
			}
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, tag+" BAD") {
				t.Fatalf("%s = %q, want %s BAD", tc.name, last, tag)
			}
			select {
			case resp := <-got:
				t.Fatalf("mechanism was given %q, want reply rejected before it", resp)
			default:
			}
			wc.mustOK(t, "NOOP")
		})
	}
}

// handleAuthenticate must not read the optional initial-resp with ExpectText,
// which swallows an unparsed non-synchronizing literal announcement ("{N+}")
// before DiscardLine can detect it and lets the literal's payload execute as
// the next command on the unauthenticated connection.
func TestAuthenticateInitialResponseNonSyncLiteralClosesWithoutSmuggling(t *testing.T) {
	const payload = "a99 LOGIN user pass\r\n"
	for _, arg := range []string{
		fmt.Sprintf("{%d+}", len(payload)),
		fmt.Sprintf("bad {%d+}", len(payload)),
		fmt.Sprintf("AG1l {%d+}", len(payload)),
		fmt.Sprintf("= {%d+}", len(payload)),
		fmt.Sprintf("x{%d+}", len(payload)),
	} {
		t.Run(arg, func(t *testing.T) {
			got := make(chan []byte, 1)
			mem := imapmemserver.New()
			mem.AddUser(newTestUser(t))
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &recordingSASLSession{Session: mem.NewSession(), got: got}, nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
				Logger:       log.New(io.Discard, "", 0),
			})
			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("net.Listen() = %v", err)
			}
			go server.Serve(ln)
			t.Cleanup(func() { server.Close() })
			wc := dialWire(t, ln.Addr().String())
			_ = wc.conn.SetDeadline(time.Now().Add(2 * time.Second))

			if _, err := fmt.Fprintf(wc.conn, "a1 AUTHENTICATE PLAIN %s\r\n%sreal NOOP\r\n", arg, payload); err != nil {
				t.Fatal(err)
			}
			_ = wc.conn.(*net.TCPConn).CloseWrite()
			raw, err := io.ReadAll(wc.br)
			if err != nil {
				t.Fatalf("reading connection close: %v (output %q)", err, raw)
			}
			out := string(raw)
			if strings.Contains(out, "a99 ") {
				t.Fatalf("literal payload was executed as a command:\n%s", out)
			}
			if !strings.Contains(out, "* BYE ") {
				t.Fatalf("unparsed non-sync literal did not close connection with BYE:\n%s", out)
			}
			if strings.Contains(out, "real ") {
				t.Fatalf("connection remained in service after desynchronization:\n%s", out)
			}
			select {
			case resp := <-got:
				t.Fatalf("mechanism was given %q, want command rejected before SASL", resp)
			default:
			}
		})
	}
}

// Validating the command line's CRLF before decoding the initial SASL response
// ensures malformed lines containing a single bare CR are rejected with tagged
// BAD at a clean command boundary regardless of whether the prefix before CR
// happens to have valid base64 length, and never execute text after the bare CR
// as a smuggled command.
func TestAuthenticateInitialResponseRefusesBareCR(t *testing.T) {
	validB64 := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
	for _, tc := range []struct {
		name        string
		initialResp string
	}{
		{name: "bare CR only", initialResp: "\r"},
		{name: "embedded bare CR at non-quantum offset", initialResp: validB64[:2] + "\r" + validB64[2:]},
		{name: "embedded bare CR at quantum offset", initialResp: validB64[:4] + "\r" + validB64[4:]},
		{name: "trailing bare CR at non-quantum offset", initialResp: "AH\r"},
		{name: "trailing bare CR", initialResp: validB64 + "\r"},
		{name: "bare CR command smuggling attempt", initialResp: "AH\ra99 LOGIN user pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan []byte, 1)
			mem := imapmemserver.New()
			mem.AddUser(newTestUser(t))
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &recordingSASLSession{Session: mem.NewSession(), got: got}, nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
				Logger:       log.New(io.Discard, "", 0),
			})
			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("net.Listen() = %v", err)
			}
			go server.Serve(ln)
			t.Cleanup(func() { server.Close() })
			wc := dialWire(t, ln.Addr().String())

			const tag = "a1"
			if _, err := io.WriteString(wc.conn, tag+" AUTHENTICATE PLAIN "+tc.initialResp+"\r\n"); err != nil {
				t.Fatal(err)
			}
			lines, err := wc.waitTagged(tag)
			if err != nil {
				t.Fatalf("waitTagged(%s) = %v", tag, err)
			}
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, tag+" BAD") {
				t.Fatalf("%s = %q, want %s BAD", tc.name, last, tag)
			}
			select {
			case resp := <-got:
				t.Fatalf("mechanism was given %q, want initial response rejected before it", resp)
			default:
			}
			wc.mustOK(t, "NOOP")
			selTag := wc.send(t, "SELECT INBOX")
			selLines, err := wc.waitTagged(selTag)
			if err != nil {
				t.Fatalf("waitTagged(SELECT) = %v", err)
			}
			if selLast := selLines[len(selLines)-1]; !strings.HasPrefix(selLast, selTag+" NO") && !strings.HasPrefix(selLast, selTag+" BAD") {
				t.Fatalf("SELECT after rejected AUTHENTICATE = %q, want NO/BAD (not authenticated)", selLast)
			}
		})
	}
}
