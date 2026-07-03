package imapclient_test

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
)

func TestStartTLS(t *testing.T) {
	conn, server := newMemClientServerPair(t)
	defer conn.Close()
	defer server.Close()

	options := imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client, err := imapclient.NewStartTLS(conn, &options)
	if err != nil {
		t.Fatalf("NewStartTLS() = %v", err)
	}
	defer client.Close()

	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop().Wait() = %v", err)
	}
}

// TestStartTLS_ServerBufferedDataBeforeOK exercises the anti-smuggling defense:
// a server that pipelines plaintext before the STARTTLS OK must be refused with
// an error — and, as a regression guard, must NOT panic on a nil *tls.Conn.
func TestStartTLS_ServerBufferedDataBeforeOK(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2 STARTTLS] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return
		}
		tag := fields[0]
		// OK plus smuggled plaintext in a single write, so it lands buffered in
		// the client's reader before the TLS handshake.
		io.WriteString(serverConn, tag+" OK begin TLS now\r\n* BYE injected\r\n")
		io.Copy(io.Discard, br)
	}()

	_, err := imapclient.NewStartTLS(clientConn, &imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	})
	if err == nil {
		t.Fatal("NewStartTLS() should fail when the server buffers data before the OK")
	}
	if !strings.Contains(err.Error(), "buffered data") {
		t.Fatalf("NewStartTLS() error = %v, want a buffered-data refusal", err)
	}
}

// TestNewStartTLS_RequiresServerName guards against silently unverified TLS:
// NewStartTLS must refuse a config that neither sets ServerName nor opts into
// InsecureSkipVerify, rather than proceeding to an opaque fail-closed handshake.
func TestNewStartTLS_RequiresServerName(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	_, err := imapclient.NewStartTLS(clientConn, &imapclient.Options{
		TLSConfig: &tls.Config{},
	})
	if err == nil {
		t.Fatal("NewStartTLS() with empty ServerName and no InsecureSkipVerify should error")
	}
	if !strings.Contains(err.Error(), "ServerName") {
		t.Fatalf("NewStartTLS() error = %v, want a ServerName requirement", err)
	}
}
