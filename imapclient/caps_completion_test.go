package imapclient

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

// Hold completion at its notification to the waiter. Capabilities from before
// LOGIN must already have been invalidated when that notification can happen;
// otherwise Login().Wait followed immediately by Caps can return the old set.
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
	// No response can arrive until allowLogin is closed. An unbuffered
	// notification lets the test inspect the state the waiter will observe.
	cmd.done = make(chan error)
	close(allowLogin)
	deadline := time.Now().Add(time.Second)
	for client.State() != imap.ConnStateAuthenticated && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
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
