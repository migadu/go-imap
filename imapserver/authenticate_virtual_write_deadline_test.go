package imapserver_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// Advance charges elapsed time to the currently installed write deadline, then
// applies the remaining budget to the real socket. Client/backend waits can
// therefore exceed 30 seconds without sleeping, and an expired write still
// fails through net.Conn's actual deadline enforcement. Fresh and cleared
// deadlines are interpreted relative to the newly advanced clock.
type authenticateWriteClockConn struct {
	net.Conn
	mu          sync.Mutex
	elapsed     time.Duration
	expires     time.Duration
	active      bool
	nextWrite   chan struct{}
	timeouts    atomic.Int32
	writeFailed chan error
}

func (c *authenticateWriteClockConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = !deadline.IsZero()
	if c.active {
		c.expires = c.elapsed + time.Until(deadline)
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *authenticateWriteClockConn) Advance(elapsed time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elapsed += elapsed
	if !c.active {
		return nil
	}
	return c.Conn.SetWriteDeadline(time.Now().Add(c.expires - c.elapsed))
}

func (c *authenticateWriteClockConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	started := c.nextWrite
	c.nextWrite = nil
	c.mu.Unlock()
	if started != nil {
		close(started)
	}
	n, err := c.Conn.Write(p)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		c.timeouts.Add(1)
		select {
		case c.writeFailed <- err:
		default:
		}
	}
	return n, err
}

type authenticateWriteClockListener struct {
	net.Listener
	accepted chan *authenticateWriteClockConn
}

func (ln authenticateWriteClockListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	clock := &authenticateWriteClockConn{Conn: conn, writeFailed: make(chan error, 1)}
	ln.accepted <- clock
	return clock, nil
}

type authenticateWritePlan struct {
	clientWait, loginWork, firstNextWork, laterNextWork, capsWork time.Duration
	customSASL, blockedChallenge                                  bool
}

type authenticateWriteSession struct {
	imapserver.Session
	clock         *authenticateWriteClockConn
	plan          authenticateWritePlan
	loggedIn      atomic.Bool
	loginFinished atomic.Bool
	nextFinished  atomic.Int32
	capsFinished  atomic.Bool
	capsOnce      sync.Once
}

func (s *authenticateWriteSession) Login(ctx context.Context, username, password string) error {
	if err := s.clock.Advance(s.plan.loginWork); err != nil {
		return err
	}
	err := s.Session.Login(ctx, username, password)
	s.loggedIn.Store(err == nil)
	s.loginFinished.Store(true)
	return err
}

func (s *authenticateWriteSession) GetCapabilities() imap.CapSet {
	if s.loggedIn.Load() {
		s.capsOnce.Do(func() {
			_ = s.clock.Advance(s.plan.capsWork)
			s.capsFinished.Store(true)
		})
	}
	return imap.CapSet{imap.CapIMAP4rev1: {}}
}

type authenticateWriteSASLSession struct{ *authenticateWriteSession }

func (s *authenticateWriteSASLSession) AuthenticateMechanisms() []string { return []string{"TEST"} }
func (s *authenticateWriteSASLSession) Authenticate(string) (sasl.Server, error) {
	return &authenticateWriteSASLServer{session: s.authenticateWriteSession}, nil
}

type authenticateWriteSASLServer struct {
	session *authenticateWriteSession
	step    int
}

func (s *authenticateWriteSASLServer) Next(response []byte) ([]byte, bool, error) {
	session := s.session
	s.step++
	switch s.step {
	case 1:
		if err := session.clock.Advance(session.plan.firstNextWork); err != nil {
			return nil, false, err
		}
		session.nextFinished.Add(1)
		if session.plan.blockedChallenge {
			return bytes.Repeat([]byte{'x'}, 4<<20), false, nil
		}
		return []byte("first"), false, nil
	case 2:
		if string(response) != "response-one" {
			return nil, false, fmt.Errorf("unexpected first response: %q", response)
		}
		if err := session.clock.Advance(session.plan.laterNextWork); err != nil {
			return nil, false, err
		}
		session.nextFinished.Add(1)
		return []byte("second"), false, nil
	case 3:
		if string(response) != "response-two" {
			return nil, false, fmt.Errorf("unexpected second response: %q", response)
		}
		err := session.Login(context.Background(), "user", "pass")
		session.nextFinished.Add(1)
		return nil, true, err
	default:
		return nil, false, errors.New("unexpected extra SASL step")
	}
}

func newAuthenticateWriteClock(t *testing.T, plan authenticateWritePlan) (*wireConn, *authenticateWriteSession, *syncTLSHookLog) {
	t.Helper()
	mem := imapmemserver.New()
	mem.AddUser(newTestUser(t))
	var logged syncTLSHookLog
	sessions := make(chan *authenticateWriteSession, 1)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := &authenticateWriteSession{Session: mem.NewSession(), clock: conn.NetConn().(*authenticateWriteClockConn), plan: plan}
			sessions <- s
			if plan.customSASL {
				return &authenticateWriteSASLSession{s}, nil, nil
			}
			return s, nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		Logger:       log.New(&logged, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *authenticateWriteClockConn, 1)
	go server.Serve(authenticateWriteClockListener{Listener: ln, accepted: accepted})
	t.Cleanup(func() { server.Close() })
	wc := dialWire(t, ln.Addr().String())
	s := <-sessions
	if clock := <-accepted; clock != s.clock {
		t.Fatal("session is not using the accepted connection")
	}
	return wc, s, &logged
}

// Only time spent writing a response should consume its write budget. Waiting
// for a SASL response, checking credentials, or calculating the next challenge
// can take longer than that budget without losing a successful authentication.
func TestAuthenticateWriteDeadlineExcludesClientAndBackendWaits(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan authenticateWritePlan
	}{
		{"client_wait_then_login", authenticateWritePlan{clientWait: 20 * time.Second, loginWork: 15 * time.Second}},
		{"SASL_IR_slow_login", authenticateWritePlan{loginWork: 35 * time.Second}},
		{"slow_first_Next", authenticateWritePlan{customSASL: true, firstNextWork: 35 * time.Second}},
		{"slow_later_Next", authenticateWritePlan{customSASL: true, laterNextWork: 35 * time.Second}},
		{"slow_final_capabilities", authenticateWritePlan{capsWork: 35 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wc, session, logged := newAuthenticateWriteClock(t, tc.plan)
			plain := base64.StdEncoding.EncodeToString([]byte("\x00user\x00pass"))
			var tag string
			if tc.plan.customSASL {
				tag = wc.send(t, "AUTHENTICATE TEST")
				for i, challenge := range []string{"first", "second"} {
					line, err := wc.readLine()
					if err != nil || line != "+ "+base64.StdEncoding.EncodeToString([]byte(challenge)) {
						t.Fatalf("challenge %d = %q, %v; completed Next calls=%d, real write timeouts=%d, logs=%q", i+1, line, err, session.nextFinished.Load(), session.clock.timeouts.Load(), logged.String())
					}
					response := []string{"response-one", "response-two"}[i]
					if _, err := io.WriteString(wc.conn, base64.StdEncoding.EncodeToString([]byte(response))+"\r\n"); err != nil {
						t.Fatal(err)
					}
				}
			} else if tc.plan.clientWait != 0 {
				tag = wc.send(t, "AUTHENTICATE PLAIN")
				if line, err := wc.readLine(); err != nil || line != "+ " {
					t.Fatalf("empty challenge = %q, %v", line, err)
				}
				if err := session.clock.Advance(tc.plan.clientWait); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(wc.conn, plain+"\r\n"); err != nil {
					t.Fatal(err)
				}
			} else {
				tag = wc.send(t, "AUTHENTICATE PLAIN "+plain)
			}
			lines, err := wc.waitTagged(tag)
			if err != nil {
				t.Fatalf("successful backend lost tagged OK: %v; login completed=%v, real write timeouts=%d, logs=%q", err, session.loginFinished.Load(), session.clock.timeouts.Load(), logged.String())
			}
			if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK ") {
				t.Fatalf("completion = %q, want tagged OK", last)
			}
			if !session.loginFinished.Load() || !session.loggedIn.Load() || !session.capsFinished.Load() {
				t.Fatal("authentication completed before backend login/capability work finished")
			}
			wc.mustOK(t, `LIST "" "*"`)
			wc.mustOK(t, "NOOP")
			if session.clock.timeouts.Load() != 0 || logged.String() != "" {
				t.Fatalf("successful authentication incurred write errors: timeouts=%d, logs=%q", session.clock.timeouts.Load(), logged.String())
			}
		})
	}
}

// Clearing deadlines around backend work must not remove the deadline from an
// actual blocked challenge write. Do not read the large challenge; advancing
// past the active write budget must unblock the real socket with a timeout.
func TestAuthenticateBlockedChallengeWriteStillTimesOut(t *testing.T) {
	wc, session, _ := newAuthenticateWriteClock(t, authenticateWritePlan{customSASL: true, blockedChallenge: true})
	started := make(chan struct{})
	session.clock.mu.Lock()
	session.clock.nextWrite = started
	session.clock.mu.Unlock()
	wc.send(t, "AUTHENTICATE TEST")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("challenge write did not start")
	}
	if err := session.clock.Advance(31 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-session.clock.writeFailed:
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("blocked write returned %v, want network timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked challenge write lost its write deadline")
	}
}
