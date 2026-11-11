package imapserver_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func startTLSHookServer(t *testing.T, options *imapserver.Options) (*imapserver.Server, net.Conn, *bufio.Reader) {
	t.Helper()
	if options.NewSession == nil {
		mem := imapmemserver.New()
		mem.AddUser(imapmemserver.NewUser("user", "pass"))
		options.NewSession = func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		}
	}
	options.Caps = imap.CapSet{imap.CapIMAP4rev1: {}}
	options.Logger = log.New(io.Discard, "", 0)
	srv := imapserver.New(options)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* OK") {
		t.Fatalf("greeting = %q, %v", line, err)
	}
	return srv, conn, br
}

func readTLSHookResponse(t *testing.T, br *bufio.Reader, tag string) string {
	t.Helper()
	var response strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading %s response: %v", tag, err)
		}
		response.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			return response.String()
		}
	}
}

// Hold Finished and the first command for one transport Write. TLS 1.3 lets
// the client complete its side before the server has read that final flight.
type heldTLSHookWrites struct {
	net.Conn
	writes int
	held   []byte
}

func (c *heldTLSHookWrites) Write(p []byte) (int, error) {
	c.writes++
	if c.writes == 1 {
		return c.Conn.Write(p)
	}
	c.held = append(c.held, p...)
	return len(p), nil
}

func (c *heldTLSHookWrites) release() error {
	_, err := c.Conn.Write(c.held)
	c.held = nil
	return err
}

type tlsHookSession struct {
	imapserver.Session
	login func()
}

func (s *tlsHookSession) Login(ctx context.Context, username, password string) error {
	s.login()
	return s.Session.Login(ctx, username, password)
}

func TestSTARTTLSHooksBracketUpgradeAndCoalescedCommand(t *testing.T) {
	starting, completing, login := make(chan struct{}), make(chan struct{}), make(chan struct{})
	allowStart, allowComplete := make(chan struct{}), make(chan struct{})
	var startOnce, completeOnce sync.Once
	defer startOnce.Do(func() { close(allowStart) })
	defer completeOnce.Do(func() { close(allowComplete) })
	var startedTransport net.Conn
	var libraryConn *imapserver.Conn
	var completed atomic.Bool
	mem := imapmemserver.New()
	mem.AddUser(imapmemserver.NewUser("user", "pass"))
	_, conn, br := startTLSHookServer(t, &imapserver.Options{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{debugTestCert(t)}},
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			libraryConn = c
			return &tlsHookSession{Session: mem.NewSession(), login: func() {
				if !completed.Load() {
					t.Error("LOGIN dispatched before the completion hook returned")
				}
				close(login)
			}}, nil, nil
		},
		OnStartTLS: func(plain net.Conn) {
			startedTransport = plain
			if plain != libraryConn.NetConn() {
				t.Error("start hook did not receive the plaintext transport")
			}
			close(starting)
			<-allowStart
		},
		OnTLSHandshakeComplete: func(plain net.Conn, secure *tls.Conn) {
			if plain != startedTransport || secure != libraryConn.NetConn() || !secure.ConnectionState().HandshakeComplete {
				t.Error("completion hook did not receive the original transport and completed TLS connection")
			}
			close(completing)
			<-allowComplete
			completed.Store(true)
		},
	})

	// Fragmented command bytes are interpreted only by the protocol parser.
	fmt.Fprint(conn, "a STAR")
	fmt.Fprint(conn, "TTLS\r\n")
	select {
	case <-starting:
	case <-time.After(2 * time.Second):
		t.Fatal("start hook not called")
	}
	ack := make(chan string, 1)
	go func() { line, _ := br.ReadString('\n'); ack <- line }()
	select {
	case line := <-ack:
		t.Fatalf("acknowledgement preceded the start hook's return: %q", line)
	case <-time.After(25 * time.Millisecond):
	}
	startOnce.Do(func() { close(allowStart) })
	if line := <-ack; !strings.HasPrefix(line, "a OK") {
		t.Fatalf("STARTTLS response = %q", line)
	}

	held := &heldTLSHookWrites{Conn: conn}
	secure := tls.Client(held, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}})
	if err := secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completing:
		t.Fatal("completion hook ran before the client's Finished was sent")
	default:
	}
	if _, err := io.WriteString(secure, "b LOGIN user pass\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := held.release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completing:
	case <-time.After(2 * time.Second):
		t.Fatal("completion hook not called")
	}
	select {
	case <-login:
		t.Fatal("coalesced LOGIN dispatched while completion hook was running")
	default:
	}
	completeOnce.Do(func() { close(allowComplete) })
	if got := readTLSHookResponse(t, bufio.NewReader(secure), "b"); !strings.Contains(got, "b OK") {
		t.Fatalf("post-TLS LOGIN response = %q", got)
	}
}

func TestSTARTTLSHooksOmitRejectedCommands(t *testing.T) {
	for _, name := range []string{"syntax", "disabled", "authenticated", "pipelined"} {
		t.Run(name, func(t *testing.T) {
			var starts, completions atomic.Int32
			opts := &imapserver.Options{
				TLSConfig:              &tls.Config{Certificates: []tls.Certificate{debugTestCert(t)}},
				InsecureAuth:           true,
				OnStartTLS:             func(net.Conn) { starts.Add(1) },
				OnTLSHandshakeComplete: func(net.Conn, *tls.Conn) { completions.Add(1) },
			}
			if name == "disabled" {
				opts.TLSConfig = nil
			}
			_, conn, br := startTLSHookServer(t, opts)
			if name == "authenticated" {
				fmt.Fprint(conn, "l LOGIN user pass\r\n")
				if got := readTLSHookResponse(t, br, "l"); !strings.Contains(got, "l OK") {
					t.Fatalf("LOGIN response = %q", got)
				}
			}
			command := "a STARTTLS\r\n"
			if name == "syntax" {
				command = "a STARTTLS extra\r\n"
			} else if name == "pipelined" {
				command += "b NOOP\r\n"
			}
			fmt.Fprint(conn, command)
			if got := readTLSHookResponse(t, br, "a"); !strings.Contains(got, "a BAD") && !strings.Contains(got, "a NO") {
				t.Fatalf("rejected STARTTLS response = %q", got)
			}
			if starts.Load() != 0 || completions.Load() != 0 {
				t.Fatalf("hooks for rejected STARTTLS: start=%d complete=%d", starts.Load(), completions.Load())
			}
		})
	}
}

func TestSTARTTLSHandshakeFailureOmitsCompletionAndPlaintextReply(t *testing.T) {
	var starts, completions atomic.Int32
	_, conn, br := startTLSHookServer(t, &imapserver.Options{
		TLSConfig:              &tls.Config{Certificates: []tls.Certificate{debugTestCert(t)}},
		OnStartTLS:             func(net.Conn) { starts.Add(1) },
		OnTLSHandshakeComplete: func(net.Conn, *tls.Conn) { completions.Add(1) },
	})
	fmt.Fprint(conn, "a STARTTLS\r\n")
	if got := readTLSHookResponse(t, br, "a"); !strings.Contains(got, "a OK") {
		t.Fatalf("STARTTLS response = %q", got)
	}
	fmt.Fprint(conn, "not TLS\r\n")
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("server wrote a protocol reply after failed TLS negotiation: %q", got)
	}
	if starts.Load() != 1 || completions.Load() != 0 {
		t.Fatalf("hooks after failed handshake: start=%d complete=%d", starts.Load(), completions.Load())
	}
}

func TestSTARTTLSHandshakeCancelledByServerClose(t *testing.T) {
	var completions atomic.Int32
	srv, conn, br := startTLSHookServer(t, &imapserver.Options{
		TLSConfig:              &tls.Config{Certificates: []tls.Certificate{debugTestCert(t)}},
		OnTLSHandshakeComplete: func(net.Conn, *tls.Conn) { completions.Add(1) },
	})
	fmt.Fprint(conn, "a STARTTLS\r\n")
	if got := readTLSHookResponse(t, br, "a"); !strings.Contains(got, "a OK") {
		t.Fatalf("STARTTLS response = %q", got)
	}
	closed := make(chan error, 1)
	go func() { closed <- srv.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server Close waited for the stalled TLS handshake")
	}
	if got, err := io.ReadAll(br); err != nil || len(got) != 0 {
		t.Fatalf("connection after Close = %q, %v", got, err)
	}
	if completions.Load() != 0 {
		t.Fatal("completion hook ran for the cancelled handshake")
	}
}
