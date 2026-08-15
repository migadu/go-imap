package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

// preGreetingServer is a server that throws away anything the client sends
// before the greeting has been written.
//
// This is not a contrived failure mode. Servers hardened against the pre-auth
// plaintext command injection class of bugs (CVE-2011-0411 and relatives)
// deliberately discard client input buffered before the session is ready,
// precisely so that injected commands cannot be replayed into the authenticated
// session. Cyrus IMAP does it, which is what emersion/go-imap#600 ran into: the
// client's LOGIN was swallowed and the server never answered its tag.
type preGreetingServer struct {
	ln net.Listener

	mu        sync.Mutex
	discarded string
}

func newPreGreetingServer(t *testing.T) *preGreetingServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	s := &preGreetingServer{ln: ln}
	t.Cleanup(func() { ln.Close() })

	go s.serve()
	return s
}

func (s *preGreetingServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Give a client that does not wait for the greeting time to get its bytes
	// into the socket, then drain and discard whatever arrived.
	time.Sleep(250 * time.Millisecond)
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 4096)
	if n, _ := conn.Read(buf); n > 0 {
		s.mu.Lock()
		s.discarded = string(buf[:n])
		s.mu.Unlock()
	}
	conn.SetReadDeadline(time.Time{})

	// Advertising the capabilities in the greeting keeps the client from
	// sending its own CAPABILITY probe, so the only thing on the wire is what
	// the test asked for.
	io.WriteString(conn, "* OK [CAPABILITY IMAP4rev2] Cyrus-like server ready\r\n")

	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag := fields[0]
		switch strings.ToUpper(fields[1]) {
		case "CAPABILITY":
			io.WriteString(conn, "* CAPABILITY IMAP4rev2\r\n")
			io.WriteString(conn, tag+" OK Completed\r\n")
		case "LOGIN":
			io.WriteString(conn, tag+" OK User logged in\r\n")
		default:
			io.WriteString(conn, tag+" OK Completed\r\n")
		}
	}
}

func (s *preGreetingServer) addr() string { return s.ln.Addr().String() }

func (s *preGreetingServer) discardedBytes() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discarded
}

// TestLoginWaitsForGreeting is the regression test for emersion/go-imap#600
// (reported by tomas-kucera against Cyrus IMAP 2.5).
//
// A client that writes a command before the greeting has arrived is speaking
// out of turn: until the greeting is read it does not know whether the
// connection is in the Not Authenticated state, is PREAUTH, or is being
// refused with BYE. Against a server that discards pre-greeting input the
// command is simply lost and its tag is never answered, so Wait blocks until
// the response timeout fires.
//
// Note that the reporter's own workaround -- calling WaitGreeting first -- is
// deliberately NOT used here. Needing it is the bug.
func TestLoginWaitsForGreeting(t *testing.T) {
	s := newPreGreetingServer(t)

	conn, err := net.Dial("tcp", s.addr())
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}

	// A short response timeout keeps the red case fast: without it this test
	// would sit on the 30s default before reporting the hang.
	client := imapclient.New(conn, &imapclient.Options{ResponseTimeout: 3 * time.Second})
	defer client.Close()

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Errorf("Login().Wait() = %v; the command was written before the greeting and the server discarded it", err)
	}

	// The precise statement of the fix: nothing at all may reach the server
	// ahead of its greeting.
	if got := s.discardedBytes(); got != "" {
		t.Errorf("client sent %q before the greeting, want nothing", got)
	}
}
