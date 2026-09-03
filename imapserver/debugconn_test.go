package imapserver_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// recorder is a DebugConn that keeps both directions apart.
type recorder struct {
	mu             sync.Mutex
	client, server strings.Builder
	peer           string
}

func (r *recorder) ClientBytes(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client.Write(p)
}

func (r *recorder) ServerBytes(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.server.Write(p)
}

func (r *recorder) snapshot() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client.String(), r.server.String()
}

func debugTestCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The point of NewDebugConn: a capture that stays PLAINTEXT across a STARTTLS
// upgrade. Nothing outside the library can do this — the upgrade happens on
// the connection the caller handed us, so any tee below it records ciphertext
// from that point on.
func TestNewDebugConnRecordsPlaintextAcrossSTARTTLS(t *testing.T) {
	memServer := imapmemserver.New()
	var rec *recorder
	var recMu sync.Mutex

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}, imap.CapStartTLS: {}},
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{debugTestCert(t)}},
		NewDebugConn: func(c net.Conn) imapserver.DebugConn {
			recMu.Lock()
			defer recMu.Unlock()
			rec = &recorder{peer: c.RemoteAddr().String()}
			return rec
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ln)
	defer server.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, err := br.ReadString('\n'); err != nil { // greeting
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("a1 STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "a1 OK") {
		t.Fatalf("STARTTLS response = %q", line)
	}
	if br.Buffered() > 0 {
		t.Fatalf("unexpected buffered data before TLS")
	}

	// Upgrade and issue a command that only exists post-TLS.
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsConn.Write([]byte("a2 CAPABILITY\r\n")); err != nil {
		t.Fatal(err)
	}
	tbr := bufio.NewReader(tlsConn)
	var got string
	for {
		l, err := tbr.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		got += l
		if strings.HasPrefix(l, "a2 ") {
			break
		}
	}
	if !strings.Contains(got, "CAPABILITY") {
		t.Fatalf("post-TLS response = %q", got)
	}
	tlsConn.Close()
	time.Sleep(50 * time.Millisecond)

	recMu.Lock()
	r := rec
	recMu.Unlock()
	if r == nil {
		t.Fatal("NewDebugConn was never called")
	}
	client, srv := r.snapshot()

	// The post-upgrade command and its response must appear as plaintext.
	if !strings.Contains(client, "a2 CAPABILITY") {
		t.Errorf("post-STARTTLS client plaintext missing; got %q", client)
	}
	if !strings.Contains(srv, "a2 ") || !strings.Contains(srv, "CAPABILITY") {
		t.Errorf("post-STARTTLS server plaintext missing; got %q", srv)
	}
	// And the directions must not be mixed: the client's command must not show
	// up in the server stream. This is what a single DebugWriter cannot give.
	if strings.Contains(srv, "a2 CAPABILITY\r\n") {
		t.Errorf("client bytes leaked into the server direction")
	}
	// No TLS record headers should reach either stream.
	if strings.Contains(client, "\x16\x03\x03") || strings.Contains(srv, "\x16\x03\x03") {
		t.Errorf("ciphertext leaked into the capture")
	}
}

// A connection NewDebugConn declines (nil) must be served normally and
// recorded nowhere — that is what makes a one-client capture possible.
func TestNewDebugConnNilRecordsNothing(t *testing.T) {
	memServer := imapmemserver.New()
	var called int
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev2: {}},
		NewDebugConn: func(net.Conn) imapserver.DebugConn { called++; return nil },
	})
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ln)
	defer server.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "* OK") {
		t.Fatalf("greeting = %q", line)
	}
	if called == 0 {
		t.Errorf("NewDebugConn was not consulted")
	}
}
