package imapclient_test

import (
	"bufio"
	"context"
	"encoding/base64"
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

func TestClient_Authenticate(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateNotAuthenticated)
	defer client.Close()
	defer server.Close()

	saslClient := sasl.NewPlainClient("", testUsername, testPassword)
	if err := client.Authenticate(saslClient); err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}

	if state := client.State(); state != imap.ConnStateAuthenticated {
		t.Errorf("State() = %v, want %v", state, imap.ConnStateAuthenticated)
	}
}

const emptyRespMech = "X-TEST-EMPTY"

// emptyRespSASLClient is a two-step mechanism whose response to the server's
// challenge is zero-length, like the mutual-authentication step of SASL GSSAPI
// (RFC 4752 Section 3.1), where the client acknowledges the server's AP-REP
// with an empty token.
type emptyRespSASLClient struct{}

func (*emptyRespSASLClient) Start() (mech string, ir []byte, err error) {
	return emptyRespMech, nil, nil
}

func (*emptyRespSASLClient) Next(challenge []byte) ([]byte, error) {
	return []byte{}, nil
}

// TestClient_Authenticate_EmptyContinuationResponse is a regression test for a
// zero-length SASL continuation response being sent as "=".
//
// RFC 4959 Section 3 defines "=" as the zero-length *initial response* marker,
// valid only in the "AUTHENTICATE <mech> <initial-response>" command line. A
// continuation response is plain base64 (RFC 3501 Section 9, base64), where a
// zero-length value is an empty line. Servers that validate base64 strictly
// (Dovecot: "Invalid base64 data in continued response") reject "=" and fail
// the authentication.
//
// Upstream report: emersion/go-imap#760, fix proposed in emersion/go-imap#761.
func TestClient_Authenticate_EmptyContinuationResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	respCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		defer serverConn.Close()

		br := bufio.NewReader(serverConn)
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2 AUTH="+emptyRespMech+"] ready\r\n"); err != nil {
			errCh <- fmt.Errorf("write greeting: %v", err)
			return
		}

		line, err := br.ReadString('\n')
		if err != nil {
			errCh <- fmt.Errorf("read AUTHENTICATE: %v", err)
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			errCh <- fmt.Errorf("empty command line")
			return
		}
		tag := fields[0]

		// Challenge the client. Its response to this challenge is zero-length.
		challenge := base64.StdEncoding.EncodeToString([]byte("challenge"))
		if _, err := io.WriteString(serverConn, "+ "+challenge+"\r\n"); err != nil {
			errCh <- fmt.Errorf("write continuation request: %v", err)
			return
		}

		resp, err := br.ReadString('\n')
		if err != nil {
			errCh <- fmt.Errorf("read SASL response: %v", err)
			return
		}
		respCh <- strings.TrimSuffix(strings.TrimSuffix(resp, "\n"), "\r")

		io.WriteString(serverConn, tag+" OK "+emptyRespMech+" authentication successful\r\n")
		io.Copy(io.Discard, br)
	}()

	client := imapclient.New(clientConn, nil)
	defer client.Close()

	if err := client.Authenticate(&emptyRespSASLClient{}); err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("test server: %v", err)
	case got := <-respCh:
		if got == "=" {
			t.Fatalf(`zero-length continuation response sent as "="; RFC 4959 reserves "=" for the initial-response slot, so this must be an empty line`)
		}
		if got != "" {
			t.Fatalf("continuation response = %q, want an empty line", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the SASL continuation response")
	}
}

type saslTestSession struct {
	imapserver.Session
	mechs map[string]func() sasl.Server
}

func (s *saslTestSession) AuthenticateMechanisms() []string {
	out := make([]string, 0, len(s.mechs))
	for m := range s.mechs {
		out = append(out, m)
	}
	return out
}

func (s *saslTestSession) Authenticate(mech string) (sasl.Server, error) {
	fn, ok := s.mechs[mech]
	if !ok {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "unsupported mech"}
	}
	return fn(), nil
}

type serverFirstEmptyChallengeClient struct{}

func (*serverFirstEmptyChallengeClient) Start() (string, []byte, error) {
	return "X-SERVER-FIRST-EMPTY", nil, nil
}

func (*serverFirstEmptyChallengeClient) Next(challenge []byte) ([]byte, error) {
	if challenge == nil || len(challenge) != 0 {
		return nil, fmt.Errorf("unexpected challenge %v", challenge)
	}
	return []byte("client-proof"), nil
}

type serverFirstEmptyChallengeServer struct {
	step    int
	onLogin func() error
}

func (s *serverFirstEmptyChallengeServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	switch s.step {
	case 1:
		if resp != nil {
			return nil, false, fmt.Errorf("unexpected initial response %q", resp)
		}
		return []byte{}, false, nil
	case 2:
		if string(resp) != "client-proof" {
			return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "bad proof"}
		}
		return nil, true, s.onLogin()
	default:
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
}

type twoStepEmptyChallengeClient struct {
	step int
}

func (*twoStepEmptyChallengeClient) Start() (string, []byte, error) {
	return "X-TWO-STEP-EMPTY", []byte("step1-client"), nil
}

func (c *twoStepEmptyChallengeClient) Next(challenge []byte) ([]byte, error) {
	c.step++
	if c.step != 1 || challenge == nil || len(challenge) != 0 {
		return nil, fmt.Errorf("unexpected step %d challenge %v", c.step, challenge)
	}
	return []byte("step2-client"), nil
}

type twoStepEmptyChallengeServer struct {
	step    int
	onLogin func() error
}

func (s *twoStepEmptyChallengeServer) Next(resp []byte) ([]byte, bool, error) {
	s.step++
	switch s.step {
	case 1:
		if resp == nil {
			return []byte{}, false, nil
		}
		if string(resp) != "step1-client" {
			return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "bad step1"}
		}
		return []byte{}, false, nil
	case 2:
		if string(resp) == "step1-client" {
			return []byte{}, false, nil
		}
		if string(resp) != "step2-client" {
			return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "bad step2"}
		}
		return nil, true, s.onLogin()
	case 3:
		if string(resp) != "step2-client" {
			return nil, false, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "bad step2"}
		}
		return nil, true, s.onLogin()
	default:
		return nil, false, sasl.ErrUnexpectedClientResponse
	}
}

// TestClient_Authenticate_EmptyServerChallenge checks that Authenticate passes
// an empty server challenge ("+ \r\n", RFC 9051 §6.2.2 and RFC 4648 §10) to
// sasl.Client.Next([]byte{}) when no unsent initial response is pending — both
// for server-first mechanisms whose first challenge is zero-length and for
// multi-step mechanisms that send an empty challenge after the initial response.
func TestClient_Authenticate_EmptyServerChallenge(t *testing.T) {
	for _, saslIR := range []bool{false, true} {
		t.Run(fmt.Sprintf("SASL-IR=%v", saslIR), func(t *testing.T) {
			caps := imap.CapSet{imap.CapIMAP4rev1: {}}
			if saslIR {
				caps[imap.CapSASLIR] = struct{}{}
			}

			newPair := func(t *testing.T) *imapclient.Client {
				t.Helper()
				mem := imapmemserver.New()
				mem.AddUser(imapmemserver.NewUser(testUsername, testPassword))
				srv := imapserver.New(&imapserver.Options{
					NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
						base := mem.NewSession()
						login := func() error {
							return base.Login(context.Background(), testUsername, testPassword)
						}
						return &saslTestSession{
							Session: base,
							mechs: map[string]func() sasl.Server{
								"X-SERVER-FIRST-EMPTY": func() sasl.Server { return &serverFirstEmptyChallengeServer{onLogin: login} },
								"X-TWO-STEP-EMPTY":     func() sasl.Server { return &twoStepEmptyChallengeServer{onLogin: login} },
							},
						}, nil, nil
					},
					InsecureAuth: true,
					Caps:         caps,
				})
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				go srv.Serve(ln)
				t.Cleanup(func() { srv.Close() })

				c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				return c
			}

			t.Run("server-first", func(t *testing.T) {
				c := newPair(t)
				if err := c.Authenticate(&serverFirstEmptyChallengeClient{}); err != nil {
					t.Fatalf("Authenticate(X-SERVER-FIRST-EMPTY) = %v, want nil", err)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatalf("Noop() after Authenticate = %v", err)
				}
			})

			t.Run("multi-step", func(t *testing.T) {
				c := newPair(t)
				if err := c.Authenticate(&twoStepEmptyChallengeClient{}); err != nil {
					t.Fatalf("Authenticate(X-TWO-STEP-EMPTY) = %v, want nil", err)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatalf("Noop() after Authenticate = %v", err)
				}
			})
		})
	}
}
