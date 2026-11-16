package imapserver_test

import (
	"context"
	"encoding/base64"
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

// writeDeadlineScale shrinks every write deadline the server arms by this
// factor, so the 30 s response write timeout lasts 500 ms in these tests. Read
// deadlines are left alone.
const writeDeadlineScale = 60

type scaledWriteDeadlineListener struct{ net.Listener }

func (l scaledWriteDeadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return scaledWriteDeadlineConn{c}, nil
}

type scaledWriteDeadlineConn struct{ net.Conn }

func (c scaledWriteDeadlineConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() {
		now := time.Now()
		t = now.Add(t.Sub(now) / writeDeadlineScale)
	}
	return c.Conn.SetWriteDeadline(t)
}

type slowLoginSession struct {
	imapserver.Session
	delay time.Duration
}

func (s slowLoginSession) Login(ctx context.Context, username, password string) error {
	time.Sleep(s.delay)
	return s.Session.Login(ctx, username, password)
}

// The response write timeout bounds a write to the client. It must not run
// while AUTHENTICATE waits for the client's response or for the login: the
// time a client takes to answer a continuation request (RFC 9051 §6.2.2) and the
// time the backend takes to check the credentials are not time spent writing.
// LOGIN arms the deadline only once the login is done; AUTHENTICATE must do the
// same for each of its writes, or a client that answers late after a slow login
// never gets its tagged OK although it is logged in.
func TestAuthenticateWriteDeadlineExcludesClientAndLogin(t *testing.T) {
	const respWriteTimeout = 30 * time.Second / writeDeadlineScale
	for _, tc := range []struct {
		name        string
		clientDelay time.Duration // before the client answers "+ "
		loginDelay  time.Duration // inside Session.Login
	}{
		{name: "client answers late", clientDelay: respWriteTimeout + 200*time.Millisecond},
		{name: "slow login", loginDelay: respWriteTimeout + 200*time.Millisecond},
		{name: "late answer then slow login", clientDelay: respWriteTimeout * 3 / 5, loginDelay: respWriteTimeout * 3 / 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged syncTLSHookLog
			mem := imapmemserver.New()
			mem.AddUser(newTestUser(t))
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return slowLoginSession{Session: mem.NewSession(), delay: tc.loginDelay}, nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
				Logger:       log.New(&logged, "", 0),
			})
			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("net.Listen() = %v", err)
			}
			go server.Serve(scaledWriteDeadlineListener{ln})
			t.Cleanup(func() { server.Close() })
			wc := dialWire(t, ln.Addr().String())

			tag := wc.send(t, "AUTHENTICATE PLAIN")
			if line, err := wc.readLine(); err != nil || line != "+ " {
				t.Fatalf("continuation request = %q, %v", line, err)
			}
			time.Sleep(tc.clientDelay)
			if _, err := io.WriteString(wc.conn, base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))+"\r\n"); err != nil {
				t.Fatal(err)
			}

			lines, err := wc.waitTagged(tag)
			if err != nil || !strings.HasPrefix(lines[len(lines)-1], tag+" OK ") {
				t.Fatalf("AUTHENTICATE answered %q, %v; want a tagged OK (server log %q)", lines, err, logged.String())
			}
			wc.mustOK(t, "SELECT INBOX")
			if out := logged.String(); out != "" {
				t.Errorf("server logged %q", out)
			}
		})
	}
}
