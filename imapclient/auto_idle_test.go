package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap/imapclient"
)

// autoIdleServer is a scripted server which supports IDLE if caps says so and
// records what it receives: command names, and DONE. A command received
// while it is idling is recorded as VIOLATION: RFC 2177 §3 forbids it.
type autoIdleServer struct {
	mutex sync.Mutex
	lines []string
	conn  net.Conn
}

func (s *autoIdleServer) received() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return strings.Join(s.lines, " ")
}

// waitFor waits until the server has received want, and returns what it
// received at the end of the wait.
func (s *autoIdleServer) waitFor(want string) (string, bool) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.received(); got == want {
			return got, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.received(), false
}

func newAutoIdleServer(t *testing.T, caps string, options *imapclient.Options) (*imapclient.Client, *autoIdleServer) {
	t.Helper()
	return newAutoIdleServerGreeting(t, "OK", caps, options)
}

func newAutoIdleServerGreeting(t *testing.T, greeting, caps string, options *imapclient.Options) (*imapclient.Client, *autoIdleServer) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	srv := &autoIdleServer{conn: serverConn}
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* "+greeting+" [CAPABILITY "+caps+"] ready\r\n")
		var idleTag string
		idling := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			name := fields[0]
			if len(fields) > 1 {
				name = fields[1]
			}
			if idling && name != "DONE" {
				name = "VIOLATION"
			}
			srv.mutex.Lock()
			srv.lines = append(srv.lines, name)
			srv.mutex.Unlock()

			switch name {
			case "IDLE":
				idleTag = fields[0]
				idling = true
				io.WriteString(serverConn, "+ idling\r\n")
			case "DONE":
				idling = false
				io.WriteString(serverConn, idleTag+" OK IDLE terminated\r\n")
			case "SELECT":
				io.WriteString(serverConn, "* 0 EXISTS\r\n"+fields[0]+" OK selected\r\n")
			case "LOGIN":
				io.WriteString(serverConn, fields[0]+" OK [CAPABILITY "+caps+"] logged in\r\n")
			default:
				io.WriteString(serverConn, fields[0]+" OK completed\r\n")
			}
		}
	}()

	client := imapclient.New(clientConn, options)
	t.Cleanup(func() { client.Close() })
	return client, srv
}

// With Options.AutoIdle the client idles while it has nothing to do, and ends
// IDLE before it sends anything else. RFC 2177 §3: "The client MUST NOT send
// a command while the server is waiting for the DONE".
//
// Upstream proposal: emersion/go-imap#622. Issue #143.
func TestAutoIdle(t *testing.T) {
	updates := make(chan uint32, 1)
	client, srv := newAutoIdleServer(t, "IMAP4rev1 IDLE", &imapclient.Options{
		AutoIdle: 100 * time.Millisecond,
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					updates <- *data.NumMessages
				}
			},
		},
	})

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// Nothing to do: IDLE starts by itself.
	if got, ok := srv.waitFor("LOGIN SELECT IDLE"); !ok {
		t.Fatalf("the server received %q, want LOGIN and then IDLE", got)
	}

	// What the server reports while idling reaches the handler.
	io.WriteString(srv.conn, "* 4 EXISTS\r\n")
	select {
	case n := <-updates:
		if n != 4 {
			t.Errorf("the handler was told of %d messages, want 4", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an update sent during the automatic IDLE did not reach the handler")
	}

	// A command ends IDLE first, and IDLE resumes after it.
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	if got, ok := srv.waitFor("LOGIN SELECT IDLE DONE NOOP IDLE"); !ok {
		t.Fatalf("the server received %q, want LOGIN SELECT IDLE DONE NOOP IDLE", got)
	}

	// Commands in a row do not let IDLE in between them.
	for i := 0; i < 3; i++ {
		if err := client.Noop().Wait(); err != nil {
			t.Fatalf("Noop() = %v", err)
		}
	}
	if got, ok := srv.waitFor("LOGIN SELECT IDLE DONE NOOP IDLE DONE NOOP NOOP NOOP IDLE"); !ok {
		t.Fatalf("the server received %q, want one IDLE after the three NOOPs", got)
	}
}

// A PREAUTH greeting already establishes the authenticated state. Automatic
// IDLE must start even if the caller never sends an initial command.
func TestAutoIdlePreauth(t *testing.T) {
	client, srv := newAutoIdleServerGreeting(t, "PREAUTH", "IMAP4rev1 IDLE", &imapclient.Options{AutoIdle: 20 * time.Millisecond})
	if err := client.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	if got, ok := srv.waitFor("IDLE"); !ok {
		t.Fatalf("after PREAUTH the server received %q, want IDLE", got)
	}
}

// No IDLE for a server that does not offer it, nor before authentication.
func TestAutoIdleNeedsCapabilityAndAuthentication(t *testing.T) {
	client, srv := newAutoIdleServer(t, "IMAP4rev1", &imapclient.Options{AutoIdle: 50 * time.Millisecond})
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := srv.received(); got != "LOGIN" {
		t.Errorf("the server received %q, want LOGIN alone: it does not offer IDLE", got)
	}

	client, srv = newAutoIdleServer(t, "IMAP4rev1 IDLE", &imapclient.Options{AutoIdle: 50 * time.Millisecond})
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := srv.received(); got != "" {
		t.Errorf("the server received %q before authentication, want nothing", got)
	}
}

// Without the option the client never idles on its own.
func TestAutoIdleOffByDefault(t *testing.T) {
	client, srv := newAutoIdleServer(t, "IMAP4rev1 IDLE", nil)
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := srv.received(); got != "LOGIN" {
		t.Errorf("the server received %q, want LOGIN alone", got)
	}
}

// Commands from several goroutines, with IDLE due at every turn: none of
// them may reach the server while it idles, and none may be held up.
func TestAutoIdleConcurrentCommands(t *testing.T) {
	client, srv := newAutoIdleServer(t, "IMAP4rev1 IDLE", &imapclient.Options{AutoIdle: time.Millisecond})
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 4*50)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				errs <- client.Noop().Wait()
				time.Sleep(time.Duration(j%4) * time.Millisecond)
			}
		}()
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatalf("commands are stuck behind the automatic IDLE; the server received %q", srv.received())
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Noop() = %v", err)
		}
	}

	got := srv.received()
	if strings.Contains(got, "VIOLATION") {
		t.Errorf("a command reached the server while it was idling: %q", got)
	}
	if n := strings.Count(got, "NOOP"); n != 200 {
		t.Errorf("the server received %d NOOPs, want 200", n)
	}
	if !strings.Contains(got, "IDLE DONE") {
		t.Errorf("IDLE never ran between the commands: %q", got)
	}
}
