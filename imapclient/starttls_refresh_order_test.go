package imapclient

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

func startTLSRefreshCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A greeting's capability refresh may still be waiting for STARTTLS's encoder.
// Such a request has not sent any bytes yet and will run over TLS, so it can
// satisfy the mandatory invalidation without scheduling another round trip.
func TestStartTLSReusesUnsentCapabilityRefresh(t *testing.T) {
	cert := startTLSRefreshCertificate(t)
	clientConn, serverConn := net.Pipe()
	seen, reply, handshake := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var replyOnce, handshakeOnce sync.Once
	releaseReply := func() { replyOnce.Do(func() { close(reply) }) }
	releaseHandshake := func() { handshakeOnce.Do(func() { close(handshake) }) }
	defer releaseReply()
	defer releaseHandshake()
	defer serverConn.Close()
	var requests atomic.Int32
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n")
		br := bufio.NewReader(serverConn)
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		close(seen)
		<-reply
		io.WriteString(serverConn, tag+" OK [CAPABILITY IMAP4rev1 LOGINDISABLED] begin TLS\r\n")
		<-handshake
		tlsConn := tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{cert}})
		br = bufio.NewReader(tlsConn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if fields[1] == "CAPABILITY" {
				requests.Add(1)
				io.WriteString(tlsConn, "* CAPABILITY IMAP4rev1 MOVE\r\n")
			}
			io.WriteString(tlsConn, fields[0]+" OK done\r\n")
		}
	}()
	client := New(clientConn, &Options{ResponseTimeout: 2 * time.Second})
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- client.startTLS(&tls.Config{InsecureSkipVerify: true}) }()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("server did not receive STARTTLS")
	}
	cmd := findPendingCmdByType[*startTLSCommand](client)
	if cmd == nil {
		t.Fatal("STARTTLS is not pending")
	}
	client.resetCaps(nil)
	client.mutex.Lock()
	refresh := client.capsRefresh
	client.mutex.Unlock()
	releaseReply()
	select {
	case <-cmd.done:
	case <-time.After(time.Second):
		t.Fatal("STARTTLS did not publish tagged completion")
	}
	client.mutex.Lock()
	duplicated := client.capsRefresh != refresh
	client.mutex.Unlock()
	if duplicated {
		t.Error("STARTTLS replaced an unsent refresh with another capability request")
	}
	releaseHandshake()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("STARTTLS did not finish its handshake")
	}
	if caps := client.Caps(); !caps.Has(imap.CapMove) || caps.Has(imap.CapLoginDisabled) {
		t.Errorf("capabilities after TLS = %v", caps)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if count := requests.Load(); count != 1 {
		t.Errorf("%d capability requests sent after TLS, want one", count)
	}
}

// Refusing the upgrade after tagged OK is a terminal protocol failure. Cancel
// before releasing the encoder so its queued refresh cannot use plaintext.
func TestStartTLSBufferedDataFailureCancelsBeforeRefresh(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n")
		br := bufio.NewReader(serverConn)
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		io.WriteString(serverConn, tag+" OK begin TLS\r\n* CAPABILITY IMAP4rev1 LOGINDISABLED\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 LOGINDISABLED\r\n"+strings.Fields(line)[0]+" OK done\r\n")
		}
	}()
	client := New(clientConn, &Options{ResponseTimeout: time.Second})
	defer client.Close()
	err := client.startTLS(&tls.Config{InsecureSkipVerify: true})
	if err == nil || !strings.Contains(err.Error(), "buffered data") {
		t.Fatalf("STARTTLS error = %v, want buffered-data refusal", err)
	}
	select {
	case <-client.closeCh:
	default:
		t.Error("failed upgrade released its encoder without canceling the connection")
	}
}

func TestStartTLSRefusalKeepsCleartextCapabilities(t *testing.T) {
	for _, status := range []string{"NO", "BAD"} {
		t.Run(status, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			go func() {
				io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n")
				br := bufio.NewReader(serverConn)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				io.WriteString(serverConn, strings.Fields(line)[0]+" "+status+" [CAPABILITY IMAP4rev1 MOVE] unavailable\r\n")
				if line, err = br.ReadString('\n'); err == nil {
					io.WriteString(serverConn, strings.Fields(line)[0]+" OK noop\r\n")
				}
			}()
			client := New(clientConn, &Options{ResponseTimeout: time.Second})
			defer client.Close()
			var imapErr *imap.Error
			if err := client.startTLS(&tls.Config{}); !errors.As(err, &imapErr) || string(imapErr.Type) != status {
				t.Fatalf("STARTTLS error = %v, want %s", err, status)
			}
			if caps := client.Caps(); !caps.Has(imap.CapMove) {
				t.Errorf("capabilities after refusal = %v", caps)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Errorf("NOOP after STARTTLS refusal: %v", err)
			}
		})
	}
}

func TestStartTLSHandshakeFailureCancelsAndClearsCapabilities(t *testing.T) {
	cert := startTLSRefreshCertificate(t)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED] ready\r\n")
		br := bufio.NewReader(serverConn)
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		io.WriteString(serverConn, strings.Fields(line)[0]+" OK [CAPABILITY IMAP4rev1 LOGINDISABLED] begin TLS\r\n")
		tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{cert}}).Handshake()
	}()
	client := New(clientConn, &Options{ResponseTimeout: time.Second})
	defer client.Close()
	if err := client.startTLS(&tls.Config{ServerName: "untrusted.test"}); err == nil {
		t.Fatal("TLS unexpectedly trusted the test certificate")
	}
	select {
	case <-client.closeCh:
	default:
		t.Error("handshake failure did not cancel before releasing the encoder")
	}
	if caps := client.Caps(); caps != nil {
		t.Errorf("handshake failure retained capabilities: %v", caps)
	}
}
