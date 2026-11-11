package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// gmailLikeServer greets without capabilities and answers LOGIN with an
// untagged CAPABILITY response and a tagged OK without a response code, as
// Gmail does. Its capabilities differ before and after LOGIN. It records the
// commands it receives.
type gmailLikeServer struct {
	mutex sync.Mutex
	cmds  []string
}

func (s *gmailLikeServer) commands() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]string(nil), s.cmds...)
}

func newGmailLikeServer(t *testing.T) (*imapclient.Client, *gmailLikeServer) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	srv := &gmailLikeServer{}
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK Gimap ready for requests\r\n")
		authenticated := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			srv.mutex.Lock()
			srv.cmds = append(srv.cmds, fields[1])
			srv.mutex.Unlock()

			if fields[1] == "LOGIN" {
				authenticated = true
			}
			var resp string
			if fields[1] == "LOGIN" || fields[1] == "CAPABILITY" {
				if authenticated {
					resp = "* CAPABILITY IMAP4rev1 IDLE MOVE SPECIAL-USE\r\n"
				} else {
					resp = "* CAPABILITY IMAP4rev1 IDLE AUTH=PLAIN\r\n"
				}
			}
			io.WriteString(serverConn, resp+fields[0]+" OK done\r\n")
		}
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	return client, srv
}

// RFC 9051 §6.2.3 lets a server spare the client a CAPABILITY command by
// putting a CAPABILITY response code in LOGIN's tagged OK. Without that code
// the client has to ask what the authenticated state offers -- once. It used
// to ask up to three times: for the greeting, again when LOGIN completed
// although that request was still unanswered, and again from Caps although a
// request was in flight.
//
// Upstream report: emersion/go-imap#561. Issue #139.
func TestCapabilityAskedOnceAfterLogin(t *testing.T) {
	client, srv := newGmailLikeServer(t)

	// The capabilities before authentication, so that the request for them
	// is over before LOGIN.
	if caps := client.Caps(); !caps.Has(imap.AuthCap("PLAIN")) || caps.Has(imap.CapSpecialUse) {
		t.Fatalf("Caps() before LOGIN = %v, want the server's capabilities for that state", caps)
	}

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	// Nothing from before LOGIN, the untagged CAPABILITY it came with
	// included, may pass for the capabilities of the authenticated state.
	if caps := client.Caps(); !caps.Has(imap.CapSpecialUse) || caps.Has(imap.AuthCap("PLAIN")) {
		t.Errorf("Caps() after LOGIN = %v, want the server's capabilities for that state", caps)
	}
	client.Caps()

	// Let a request that should not have been sent reach the server.
	time.Sleep(200 * time.Millisecond)

	// One request for each state, and LOGIN between them.
	if cmds := srv.commands(); strings.Join(cmds, " ") != "CAPABILITY LOGIN CAPABILITY" {
		t.Errorf("the client sent %v, want CAPABILITY, LOGIN, CAPABILITY", cmds)
	}
}

// LOGIN straight after connecting: the request the greeting set off may go
// out before LOGIN or after it. After it, it already asks about the
// authenticated state and is not repeated; before it, it is.
func TestCapabilityRefreshAroundImmediateLogin(t *testing.T) {
	client, srv := newGmailLikeServer(t)

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if caps := client.Caps(); !caps.Has(imap.CapSpecialUse) || caps.Has(imap.AuthCap("PLAIN")) {
		t.Errorf("Caps() after LOGIN = %v, want the server's capabilities for that state", caps)
	}
	time.Sleep(200 * time.Millisecond)

	switch cmds := strings.Join(srv.commands(), " "); cmds {
	case "LOGIN CAPABILITY", "CAPABILITY LOGIN CAPABILITY":
	default:
		t.Errorf("the client sent %q, want one CAPABILITY after LOGIN and at most one before", cmds)
	}
}
