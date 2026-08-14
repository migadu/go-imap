package imapclient_test

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
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
