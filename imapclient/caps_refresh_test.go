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
	// bareLogin makes it answer LOGIN with the tagged OK alone.
	bareLogin bool
}

func (s *gmailLikeServer) commands() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]string(nil), s.cmds...)
}

func newGmailLikeServer(t *testing.T, bareLogin bool) (*imapclient.Client, *gmailLikeServer) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	srv := &gmailLikeServer{bareLogin: bareLogin}
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
			if fields[1] == "CAPABILITY" || (fields[1] == "LOGIN" && !srv.bareLogin) {
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

// RFC 9051 §6.2.3 has a server spare the client a CAPABILITY command by
// putting a CAPABILITY response code in LOGIN's tagged OK. Gmail, and older
// Dovecot, send an untagged CAPABILITY response ahead of a bare tagged OK
// instead. That response cannot be the answer to anything else when LOGIN is
// the oldest command in flight, so the client takes it for what the server
// means by it and does not ask again.
//
// Upstream report: emersion/go-imap#561. Issue #139.
func TestCapabilitySentWithLogin(t *testing.T) {
	client, srv := newGmailLikeServer(t, false)

	// The capabilities before authentication, so that the request for them
	// is over before LOGIN.
	if caps := client.Caps(); !caps.Has(imap.AuthCap("PLAIN")) || caps.Has(imap.CapSpecialUse) {
		t.Fatalf("Caps() before LOGIN = %v, want the server's capabilities for that state", caps)
	}

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if caps := client.Caps(); !caps.Has(imap.CapSpecialUse) || caps.Has(imap.AuthCap("PLAIN")) {
		t.Errorf("Caps() after LOGIN = %v, want the server's capabilities for that state", caps)
	}
	client.Caps()

	// Let a request that should not have been sent reach the server.
	time.Sleep(200 * time.Millisecond)

	if cmds := srv.commands(); strings.Join(cmds, " ") != "CAPABILITY LOGIN" {
		t.Errorf("the client sent %v, want CAPABILITY and LOGIN and nothing after", cmds)
	}
}

// An untagged CAPABILITY that arrives while LOGIN is in flight but answers a
// CAPABILITY command sent ahead of it tells of the state before LOGIN. It
// must not pass for the capabilities of the authenticated state.
func TestCapabilityPipelinedAheadOfLogin(t *testing.T) {
	client, srv := newGmailLikeServer(t, true)

	client.Caps()
	capCmd := client.Capability()
	loginCmd := client.Login("user", "pass")
	if caps, err := capCmd.Wait(); err != nil || !caps.Has(imap.AuthCap("PLAIN")) {
		t.Fatalf("Capability() = %v, %v; want the capabilities before LOGIN", caps, err)
	}
	if err := loginCmd.Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if caps := client.Caps(); !caps.Has(imap.CapSpecialUse) || caps.Has(imap.AuthCap("PLAIN")) {
		t.Errorf("Caps() after LOGIN = %v, want the server's capabilities for that state", caps)
	}
	time.Sleep(200 * time.Millisecond)

	if cmds := srv.commands(); strings.Join(cmds, " ") != "CAPABILITY CAPABILITY LOGIN CAPABILITY" {
		t.Errorf("the client sent %v, want one CAPABILITY after LOGIN", cmds)
	}
}

// LOGIN straight after connecting: the request the greeting set off may go
// out before LOGIN, after it, or not at all if LOGIN's capabilities arrive
// first. Nothing is asked on top of it.
func TestCapabilityRefreshAroundImmediateLogin(t *testing.T) {
	client, srv := newGmailLikeServer(t, false)

	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if caps := client.Caps(); !caps.Has(imap.CapSpecialUse) || caps.Has(imap.AuthCap("PLAIN")) {
		t.Errorf("Caps() after LOGIN = %v, want the server's capabilities for that state", caps)
	}
	time.Sleep(200 * time.Millisecond)

	switch cmds := strings.Join(srv.commands(), " "); cmds {
	case "LOGIN", "LOGIN CAPABILITY", "CAPABILITY LOGIN":
	default:
		t.Errorf("the client sent %q, want the request the greeting set off and no other", cmds)
	}
}
