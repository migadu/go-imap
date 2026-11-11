package imapclient

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// Capabilities from before LOGIN must be invalidated before its completion is
// published, so an immediate Caps call cannot return the pre-authentication set.
func TestLoginInvalidatesCapsBeforeCompletion(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	allowLogin := make(chan struct{})
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready\r\n")
		br := bufio.NewReader(serverConn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if fields[1] == "LOGIN" {
				<-allowLogin
			} else if fields[1] == "CAPABILITY" {
				io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 MOVE\r\n")
			}
			io.WriteString(serverConn, fields[0]+" OK done\r\n")
		}
	}()
	client := New(clientConn, nil)
	defer client.Close()
	cmd := client.Login("user", "pass")
	close(allowLogin)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	client.mutex.Lock()
	stale := client.caps.Has(imap.AuthCap("PLAIN"))
	client.mutex.Unlock()
	if stale {
		t.Error("LOGIN can complete while pre-authentication capabilities remain cached")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if caps := client.Caps(); !caps.Has(imap.CapMove) || caps.Has(imap.AuthCap("PLAIN")) {
		t.Errorf("Caps() after LOGIN = %v, want only the authenticated capabilities", caps)
	}
}
