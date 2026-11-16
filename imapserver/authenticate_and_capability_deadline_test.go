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
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

const saslStepDeadlineScale = 60 // 30s cmdReadTimeout / respWriteTimeout -> 500ms

type scaledBothDeadlinesListener struct{ net.Listener }

func (l scaledBothDeadlinesListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return scaledBothDeadlinesConn{c}, nil
}

type scaledBothDeadlinesConn struct{ net.Conn }

func (c scaledBothDeadlinesConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() {
		now := time.Now()
		if d := t.Sub(now); d > 0 {
			t = now.Add(d / saslStepDeadlineScale)
		}
	}
	return c.Conn.SetReadDeadline(t)
}

func (c scaledBothDeadlinesConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() {
		now := time.Now()
		if d := t.Sub(now); d > 0 {
			t = now.Add(d / saslStepDeadlineScale)
		}
	}
	return c.Conn.SetWriteDeadline(t)
}

type multiStepSASLDeadlineSession struct {
	imapserver.Session
	firstNextDelay time.Duration
	laterNextDelay time.Duration
}

func (s *multiStepSASLDeadlineSession) AuthenticateMechanisms() []string {
	return []string{"TEST"}
}

func (s *multiStepSASLDeadlineSession) Authenticate(mech string) (sasl.Server, error) {
	if mech != "TEST" {
		return nil, fmt.Errorf("unexpected mechanism %q", mech)
	}
	return &multiStepSASLDeadlineServer{sess: s}, nil
}

type multiStepSASLDeadlineServer struct {
	sess *multiStepSASLDeadlineSession
	step int
}

func (s *multiStepSASLDeadlineServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	switch s.step {
	case 1:
		time.Sleep(s.sess.firstNextDelay)
		return []byte("first"), false, nil
	case 2:
		if string(resp) != "response-one" {
			return nil, false, fmt.Errorf("unexpected step 1 resp %q", resp)
		}
		time.Sleep(s.sess.laterNextDelay)
		return []byte("second"), false, nil
	case 3:
		if string(resp) != "response-two" {
			return nil, false, fmt.Errorf("unexpected step 2 resp %q", resp)
		}
		return nil, true, s.sess.Session.Login(context.Background(), "user", "pass")
	default:
		return nil, false, errors.New("unexpected extra SASL step")
	}
}

func TestAuthenticateReadDeadlineScopedPerStep(t *testing.T) {
	const scaledReadBudget = 30 * time.Second / saslStepDeadlineScale // 500ms

	for _, tc := range []struct {
		name            string
		firstNextDelay  time.Duration
		clientStep1Wait time.Duration
		laterNextDelay  time.Duration
		clientStep2Wait time.Duration
	}{
		{
			name:           "slow_first_Next",
			firstNextDelay: scaledReadBudget + 150*time.Millisecond,
		},
		{
			name:           "slow_later_Next",
			laterNextDelay: scaledReadBudget + 150*time.Millisecond,
		},
		{
			name:            "multi_step_cumulative_client_waits",
			clientStep1Wait: scaledReadBudget * 3 / 5,
			clientStep2Wait: scaledReadBudget * 3 / 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged syncTLSHookLog
			mem := imapmemserver.New()
			mem.AddUser(newTestUser(t))
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &multiStepSASLDeadlineSession{
						Session:        mem.NewSession(),
						firstNextDelay: tc.firstNextDelay,
						laterNextDelay: tc.laterNextDelay,
					}, nil, nil
				},
				InsecureAuth: true,
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
				Logger:       log.New(&logged, "", 0),
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go server.Serve(scaledBothDeadlinesListener{ln})
			t.Cleanup(func() { server.Close() })

			wc := dialWire(t, ln.Addr().String())
			tag := wc.send(t, "AUTHENTICATE TEST")

			waits := []time.Duration{tc.clientStep1Wait, tc.clientStep2Wait}
			challenges := []string{"first", "second"}
			responses := []string{"response-one", "response-two"}
			for i := range challenges {
				line, err := wc.readLine()
				want := "+ " + base64.StdEncoding.EncodeToString([]byte(challenges[i]))
				if err != nil || line != want {
					t.Fatalf("step %d challenge = %q, %v; want %q (logs=%q)", i+1, line, err, want, logged.String())
				}
				time.Sleep(waits[i])
				encoded := base64.StdEncoding.EncodeToString([]byte(responses[i]))
				if _, err := io.WriteString(wc.conn, encoded+"\r\n"); err != nil {
					t.Fatalf("step %d write = %v", i+1, err)
				}
			}

			lines, err := wc.waitTagged(tag)
			if err != nil || !strings.HasPrefix(lines[len(lines)-1], tag+" OK ") {
				t.Fatalf("AUTHENTICATE got %q, %v; want %s OK (logs=%q)", lines, err, tag, logged.String())
			}
			wc.mustOK(t, "NOOP")
		})
	}
}

func TestLoginAndCapabilityWriteDeadlineExcludesBackendCaps(t *testing.T) {
	t.Run("LOGIN_slow_final_capabilities", func(t *testing.T) {
		wc, session, logged := newAuthenticateWriteClock(t, authenticateWritePlan{
			capsWork: 35 * time.Second,
		})
		tag := wc.send(t, "LOGIN user pass")
		lines, err := wc.waitTagged(tag)
		if err != nil {
			t.Fatalf("LOGIN lost tagged OK: %v; loginFinished=%v, capsFinished=%v, timeouts=%d, logs=%q",
				err, session.loginFinished.Load(), session.capsFinished.Load(), session.clock.timeouts.Load(), logged.String())
		}
		if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK ") {
			t.Fatalf("LOGIN completion = %q, want tagged OK", last)
		}
		wc.mustOK(t, "NOOP")
	})

	t.Run("CAPABILITY_slow_backend_capabilities", func(t *testing.T) {
		wc, session, logged := newAuthenticateWriteClock(t, authenticateWritePlan{
			capsWork: 35 * time.Second,
		})
		session.loggedIn.Store(true)
		tag := wc.send(t, "CAPABILITY")
		lines, err := wc.waitTagged(tag)
		if err != nil {
			t.Fatalf("CAPABILITY failed under slow GetCapabilities: %v; capsFinished=%v, timeouts=%d, logs=%q",
				err, session.capsFinished.Load(), session.clock.timeouts.Load(), logged.String())
		}
		if last := lines[len(lines)-1]; !strings.HasPrefix(last, tag+" OK ") {
			t.Fatalf("CAPABILITY completion = %q, want tagged OK", last)
		}
	})
}
