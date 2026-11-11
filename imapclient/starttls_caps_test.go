package imapclient_test

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 9051 §6.2.1: "Once TLS has been started, the client MUST discard cached
// information about server capabilities and SHOULD reissue the CAPABILITY
// command. This is necessary to protect against active attacks that alter the
// capabilities list prior to STARTTLS." STARTTLS's OK is the last response
// sent in the clear, so a [CAPABILITY] on it, or a CAPABILITY response ahead
// of it, is no better than the greeting's. Issue #166.
func TestStartTLSDiscardsCleartextCapabilities(t *testing.T) {
	cert, err := tls.X509KeyPair([]byte(rsaCertPEM), []byte(rsaKeyPEM))
	if err != nil {
		t.Fatalf("tls.X509KeyPair() = %v", err)
	}
	const cleartext = "IMAP4rev1 STARTTLS LOGINDISABLED"
	for _, tc := range []struct {
		name   string
		answer func(tag string) string
	}{
		{"in the OK", func(tag string) string {
			return tag + " OK [CAPABILITY " + cleartext + "] Begin TLS\r\n"
		}},
		{"before the OK", func(tag string) string {
			return "* CAPABILITY " + cleartext + "\r\n" + tag + " OK Begin TLS\r\n"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			go func() {
				io.WriteString(serverConn, "* OK [CAPABILITY "+cleartext+"] ready\r\n")
				line, err := bufio.NewReader(serverConn).ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				io.WriteString(serverConn, tc.answer(tag))

				tlsConn := tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{cert}})
				br := bufio.NewReader(tlsConn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					tag, cmd, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
					if cmd == "CAPABILITY" {
						io.WriteString(tlsConn, "* CAPABILITY IMAP4rev1 AUTH=PLAIN MOVE\r\n")
					}
					io.WriteString(tlsConn, tag+" OK done\r\n")
				}
			}()

			client, err := imapclient.NewStartTLS(clientConn, &imapclient.Options{
				TLSConfig: &tls.Config{InsecureSkipVerify: true},
			})
			if err != nil {
				t.Fatalf("NewStartTLS() = %v", err)
			}
			defer client.Close()
			if caps := client.Caps(); caps.Has(imap.CapLoginDisabled) || !caps.Has(imap.CapMove) {
				t.Errorf("Caps() after STARTTLS = %v, want those sent over TLS", caps)
			}
		})
	}
}
