package imapclient_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type cancelAuthServerSession struct {
	imapserver.Session
}

func (s *cancelAuthServerSession) AuthenticateMechanisms() []string {
	return []string{"OAUTHBEARER", "X-UNEXPECTED-EMPTY"}
}

func (s *cancelAuthServerSession) Authenticate(mech string) (sasl.Server, error) {
	switch mech {
	case "OAUTHBEARER":
		return sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
			if opts.Token != "fresh-valid-token" {
				return &sasl.OAuthBearerError{
					Status:  "invalid_token",
					Schemes: "bearer",
				}
			}
			if err := s.Session.Login(context.Background(), opts.Username, testPassword); err != nil {
				return &sasl.OAuthBearerError{Status: "invalid_token"}
			}
			return nil
		}), nil
	case "X-UNEXPECTED-EMPTY":
		return &unexpectedEmptyChallengeServer{}, nil
	default:
		return nil, fmt.Errorf("unsupported mechanism %q", mech)
	}
}

type unexpectedEmptyChallengeServer struct {
	step int
}

func (s *unexpectedEmptyChallengeServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	if s.step == 1 {
		return []byte{}, false, nil
	}
	return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "auth failed"}
}

type rejectEmptyChallengeClient struct{}

func (*rejectEmptyChallengeClient) Start() (string, []byte, error) {
	return "X-UNEXPECTED-EMPTY", []byte("initial"), nil
}

func (*rejectEmptyChallengeClient) Next(challenge []byte) ([]byte, error) {
	return nil, sasl.ErrUnexpectedServerChallenge
}

func TestClient_Authenticate_CancelOnClientError(t *testing.T) {
	newClientAgainstServer := func(t *testing.T) *imapclient.Client {
		t.Helper()
		mem := imapmemserver.New()
		user := imapmemserver.NewUser(testUsername, testPassword)
		mem.AddUser(user)

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &cancelAuthServerSession{Session: mem.NewSession()}, nil, nil
			},
			InsecureAuth: true,
			Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })

		c, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{
			ResponseTimeout: 1 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}

	t.Run("OAUTHBEARER_expired_token_then_retry", func(t *testing.T) {
		c := newClientAgainstServer(t)

		expired := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: testUsername,
			Token:    "expired-token",
		})
		err := c.Authenticate(expired)
		var oauthErr *sasl.OAuthBearerError
		if !errors.As(err, &oauthErr) {
			t.Fatalf("Authenticate(expired) = %v, want *sasl.OAuthBearerError", err)
		}

		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("Noop() after cancelled OAUTHBEARER failed: %v", err)
		}
		fresh := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: testUsername,
			Token:    "fresh-valid-token",
		})
		if err := c.Authenticate(fresh); err != nil {
			t.Fatalf("Authenticate(fresh) after OAUTHBEARER retry failed: %v", err)
		}
	})

	t.Run("unexpected_empty_challenge_cancelled_cleanly", func(t *testing.T) {
		c := newClientAgainstServer(t)

		err := c.Authenticate(&rejectEmptyChallengeClient{})
		if !errors.Is(err, sasl.ErrUnexpectedServerChallenge) {
			t.Fatalf("Authenticate() = %v, want ErrUnexpectedServerChallenge", err)
		}

		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("Noop() after aborted Authenticate failed: %v", err)
		}
		if err := c.Login(testUsername, testPassword).Wait(); err != nil {
			t.Fatalf("Login() after aborted Authenticate failed: %v", err)
		}
	})
}

type scaledClientWriteConn struct {
	net.Conn
	scale time.Duration
}

func (c scaledClientWriteConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() {
		now := time.Now()
		if d := t.Sub(now); d > 0 {
			t = now.Add(d / c.scale)
		}
	}
	return c.Conn.SetWriteDeadline(t)
}

type slowClientStepSASL struct {
	delay time.Duration
}

func (*slowClientStepSASL) Start() (string, []byte, error) {
	return "X-SERVER-FIRST-EMPTY", nil, nil
}

func (s *slowClientStepSASL) Next(challenge []byte) ([]byte, error) {
	time.Sleep(s.delay)
	return []byte("client-proof"), nil
}

func TestClient_Authenticate_WriteDeadlineScopedPerStep(t *testing.T) {
	const scale = 60
	const scaledWriteBudget = 30 * time.Second / scale // 500ms

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveEmptyChallengeTest(serverConn, true)
	}()

	c := imapclient.New(scaledClientWriteConn{Conn: clientConn, scale: scale}, &imapclient.Options{
		ResponseTimeout: 3 * time.Second,
	})
	defer c.Close()

	if err := c.Authenticate(&slowClientStepSASL{delay: scaledWriteBudget + 200*time.Millisecond}); err != nil {
		t.Fatalf("Authenticate() failed with expired write deadline during SASL calculation: %v", err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
}
