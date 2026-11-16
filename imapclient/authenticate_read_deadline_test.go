package imapclient_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type scaledBothClientConn struct {
	net.Conn
	scale time.Duration
}

func (c scaledBothClientConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() {
		now := time.Now()
		if d := t.Sub(now); d > 0 {
			t = now.Add(d / c.scale)
		}
	}
	return c.Conn.SetReadDeadline(t)
}

func (c scaledBothClientConn) SetWriteDeadline(t time.Time) error {
	if !t.IsZero() {
		now := time.Now()
		if d := t.Sub(now); d > 0 {
			t = now.Add(d / c.scale)
		}
	}
	return c.Conn.SetWriteDeadline(t)
}

type reproSASLSession struct {
	imapserver.Session
	finalDelay time.Duration
}

func (s *reproSASLSession) AuthenticateMechanisms() []string {
	return []string{"X-DELAYED"}
}

func (s *reproSASLSession) Authenticate(mech string) (sasl.Server, error) {
	if mech != "X-DELAYED" {
		return nil, fmt.Errorf("unexpected mech %q", mech)
	}
	return &reproSASLServer{sess: s}, nil
}

type reproSASLServer struct {
	sess *reproSASLSession
	step int
}

func (s *reproSASLServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	switch s.step {
	case 1:
		return []byte("server-challenge"), false, nil
	case 2:
		time.Sleep(s.sess.finalDelay)
		if string(resp) != "client-proof" {
			return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "auth failed"}
		}
		return nil, true, s.sess.Session.Login(context.Background(), testUsername, testPassword)
	default:
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
}

type reproSASLClient struct {
	delay time.Duration
	err   error
}

func (*reproSASLClient) Start() (string, []byte, error) {
	return "X-DELAYED", nil, nil
}

func (c *reproSASLClient) Next(challenge []byte) ([]byte, error) {
	time.Sleep(c.delay)
	if c.err != nil {
		return nil, c.err
	}
	return []byte("client-proof"), nil
}

func TestClient_Authenticate_ReadDeadlineScopedPerStep(t *testing.T) {
	t.Run("default_options_slow_client_step_exceeds_30s", func(t *testing.T) {
		const scale = 60
		const scaledBudget = 30 * time.Second / scale // 500ms (both cmdWriteTimeout and respReadTimeout)

		clientConn, serverConn := net.Pipe()
		t.Cleanup(func() {
			clientConn.Close()
			serverConn.Close()
		})
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
		go func() {
			_ = serveEmptyChallengeTest(serverConn, true)
		}()

		c := imapclient.New(scaledBothClientConn{Conn: clientConn, scale: scale}, nil)
		defer c.Close()

		if err := c.Authenticate(&slowClientStepSASL{delay: scaledBudget + 200*time.Millisecond}); err != nil {
			t.Fatalf("Authenticate() with default Options failed during slow client SASL step: %v", err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("Noop() = %v", err)
		}
	})

	t.Run("client_step_consumes_server_ResponseTimeout_budget", func(t *testing.T) {
		mem := imapmemserver.New()
		mem.AddUser(imapmemserver.NewUser(testUsername, testPassword))

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &reproSASLSession{
					Session:    mem.NewSession(),
					finalDelay: 250 * time.Millisecond,
				}, nil, nil
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
			ResponseTimeout: 400 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })

		if err := c.Authenticate(&reproSASLClient{delay: 250 * time.Millisecond}); err != nil {
			t.Fatalf("Authenticate() failed even though each peer responded within ResponseTimeout: %v", err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("Noop() = %v", err)
		}
	})

	t.Run("cancelAuthenticate_within_ResponseTimeout_still_tears_down_connection", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		t.Cleanup(func() {
			clientConn.Close()
			serverConn.Close()
		})
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(serverConn)
			_, _ = io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 AUTH=X-DELAYED] ready\r\n")
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag := strings.Fields(line)[0]
			_, _ = io.WriteString(serverConn, "+ "+base64.StdEncoding.EncodeToString([]byte("challenge"))+"\r\n")
			if _, err := br.ReadString('\n'); err != nil { // reads "*\r\n"
				return
			}
			time.Sleep(250 * time.Millisecond) // < 400ms ResponseTimeout
			_, _ = io.WriteString(serverConn, tag+" BAD AUTHENTICATE cancelled\r\n")
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				fields := strings.Fields(line)
				_, _ = io.WriteString(serverConn, fields[0]+" OK NOOP completed\r\n")
			}
		}()

		c := imapclient.New(clientConn, &imapclient.Options{
			ResponseTimeout: 400 * time.Millisecond,
		})
		defer c.Close()

		// Client takes 250ms (< 400ms) before returning SASL error; server takes
		// 250ms (< 400ms) to reply BAD to "*\r\n". Because writeSASLLine("*")
		// never refreshes ResponseTimeout, cmd.wait() expires 150ms after "*" is
		// sent, closes the connection, discards the error, and breaks Noop().
		err := c.Authenticate(&reproSASLClient{
			delay: 250 * time.Millisecond,
			err:   sasl.ErrUnexpectedServerChallenge,
		})
		if !errors.Is(err, sasl.ErrUnexpectedServerChallenge) {
			t.Fatalf("Authenticate() = %v, want ErrUnexpectedServerChallenge", err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("Noop() after cancelled Authenticate failed on dead connection: %v", err)
		}
	})
}
