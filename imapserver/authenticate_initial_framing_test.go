package imapserver_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type initialFramingSession struct {
	imapserver.Session
	starts    atomic.Int32
	logins    atomic.Int32
	emptySeen atomic.Bool
}

var _ imapserver.SessionSASL = (*initialFramingSession)(nil)

func (s *initialFramingSession) AuthenticateMechanisms() []string {
	return []string{"PLAIN", "EMPTY"}
}

func (s *initialFramingSession) Login(ctx context.Context, username, password string) error {
	s.logins.Add(1)
	return s.Session.Login(ctx, username, password)
}

func (s *initialFramingSession) Authenticate(mech string) (sasl.Server, error) {
	s.starts.Add(1)
	if mech == "EMPTY" {
		return &initialFramingEmptyServer{session: s}, nil
	}
	if mech != "PLAIN" {
		return nil, fmt.Errorf("unexpected test mechanism %q", mech)
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		return s.Login(context.Background(), username, password)
	}), nil
}

// EMPTY authenticates the fixture user only on an explicitly empty response.
type initialFramingEmptyServer struct{ session *initialFramingSession }

func (s *initialFramingEmptyServer) Next(response []byte) ([]byte, bool, error) {
	explicitEmpty := response != nil && len(response) == 0
	s.session.emptySeen.Store(explicitEmpty)
	if !explicitEmpty {
		return nil, false, fmt.Errorf("expected an explicitly empty initial response")
	}
	return nil, true, s.session.Login(context.Background(), "user", "pass")
}

func newInitialFramingConn(t *testing.T) (*wireConn, *initialFramingSession) {
	t.Helper()
	mem := imapmemserver.New()
	mem.AddUser(newTestUser(t))
	sess := &initialFramingSession{Session: mem.NewSession()}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return sess, nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapLiteralPlus: {}, imap.Cap("AUTH=EMPTY"): {}},
		Logger:       log.New(io.Discard, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); srv.Close() })
	go srv.Serve(ln)
	return dialWire(t, ln.Addr().String()), sess
}

func readInitialFramingToClose(t *testing.T, wc *wireConn) string {
	t.Helper()
	if err := wc.conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := wc.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(wc.br)
	var netErr net.Error
	if err != nil {
		// The peer may close with already-sent input unread. A reset still
		// proves closure, while a timeout would leave framing unresolved.
		if !errors.As(err, &netErr) || netErr.Timeout() {
			t.Fatalf("reading connection close: %v (output %q)", err, out)
		}
	}
	return string(out)
}

// RFC 7888 section 3 makes every trailing {N+} part of the command's framing,
// even if the AUTHENTICATE arguments preceding it are already invalid. Its
// payload must never become a LOGIN command on the unauthenticated connection.
func TestAuthenticateInitialResponseLiteralPayloadNeverAuthenticates(t *testing.T) {
	payload := "injected LOGIN user pass\r\n"
	for _, prefix := range []string{"", "bad ", "= ", "x"} {
		t.Run(fmt.Sprintf("prefix_%q", prefix), func(t *testing.T) {
			wc, sess := newInitialFramingConn(t)
			if _, err := fmt.Fprintf(wc.conn, "auth AUTHENTICATE PLAIN %s{%d+}\r\n%sprobe NOOP\r\n", prefix, len(payload), payload); err != nil {
				t.Fatal(err)
			}
			out := readInitialFramingToClose(t, wc)
			if strings.Contains(out, "injected ") || strings.Contains(out, "probe ") {
				t.Errorf("literal payload or following command executed:\n%s", out)
			}
			if !strings.Contains(out, "* BYE ") {
				t.Errorf("unparsed non-synchronizing literal did not end the connection with BYE:\n%s", out)
			}
			if sess.starts.Load() != 0 || sess.logins.Load() != 0 {
				t.Errorf("invalid AUTHENTICATE called backend: Authenticate=%d Login=%d", sess.starts.Load(), sess.logins.Load())
			}
		})
	}
}

// Once the whole malformed physical line can be consumed, the response is BAD
// and a real command on the next line stays usable. Base64 decoding must not
// change recovery depending on whether its prefix has a complete quantum.
func TestAuthenticateInitialResponseMalformedLinePreservesNextCommand(t *testing.T) {
	for _, prefix := range []string{"A", "AH", "AG1", "AG1l", "!!!!", "="} {
		for _, ending := range []string{"\r\n", "\r\r\n", "\rignored\r\n"} {
			// '=' is a valid empty initial response on a normal line, and
			// AG1l is valid base64; only their malformed framing belongs here.
			if ending == "\r\n" && (prefix == "=" || prefix == "AG1l") {
				continue
			}
			t.Run(fmt.Sprintf("%q_%q", prefix, ending), func(t *testing.T) {
				wc, sess := newInitialFramingConn(t)
				if _, err := fmt.Fprintf(wc.conn, "auth AUTHENTICATE PLAIN %s%sprobe NOOP\r\n", prefix, ending); err != nil {
					t.Fatal(err)
				}
				out := readInitialFramingToClose(t, wc)
				if !strings.Contains(out, "auth BAD ") || !strings.Contains(out, "probe OK ") || strings.Contains(out, "* BYE ") {
					t.Errorf("malformed complete line did not give BAD then NOOP OK:\n%s", out)
				}
				if sess.starts.Load() != 0 || sess.logins.Load() != 0 {
					t.Errorf("malformed initial response reached backend: Authenticate=%d Login=%d", sess.starts.Load(), sess.logins.Load())
				}
			})
		}
	}
}

func TestAuthenticateInitialResponseFramingValidControls(t *testing.T) {
	for _, mode := range []string{"initial response", "no initial response", "explicit empty response"} {
		t.Run(mode, func(t *testing.T) {
			wc, sess := newInitialFramingConn(t)
			plain := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
			cmd := "AUTHENTICATE PLAIN"
			switch mode {
			case "initial response":
				cmd += " " + plain
			case "explicit empty response":
				cmd = "AUTHENTICATE EMPTY ="
			}
			tag := wc.send(t, cmd)
			if mode == "no initial response" {
				line, err := wc.br.ReadString('\n')
				if err != nil || line != "+ \r\n" {
					t.Fatalf("empty challenge = %q, %v", line, err)
				}
				if _, err := fmt.Fprint(wc.conn, plain+"\r\n"); err != nil {
					t.Fatal(err)
				}
			}
			lines, err := wc.waitTagged(tag)
			if err != nil || !strings.HasPrefix(lines[len(lines)-1], tag+" OK ") {
				t.Fatalf("valid AUTHENTICATE = %q, %v", lines, err)
			}
			wc.mustOK(t, "NOOP")
			if mode == "explicit empty response" {
				if !sess.emptySeen.Load() {
					t.Error("'=' did not reach SASL as a non-nil empty response")
				}
			}
			if sess.starts.Load() != 1 || sess.logins.Load() != 1 {
				t.Errorf("valid AUTHENTICATE backend calls: Authenticate=%d Login=%d", sess.starts.Load(), sess.logins.Load())
			}
		})
	}
}
