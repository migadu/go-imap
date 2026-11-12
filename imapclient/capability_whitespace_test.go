package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestCapabilityTrailingSpaces(t *testing.T) {
	for _, ending := range []string{"\r\n", "\n"} {
		for _, spaces := range []int{0, 1, 2, 8} {
			t.Run(fmt.Sprintf("ending=%q/spaces=%d", ending, spaces), func(t *testing.T) {
				response := "* CAPABILITY imap4rev1   IDLE  ACL rights=kxte" + strings.Repeat(" ", spaces) + ending
				client := newCodeServer(t, response)
				caps, err := client.Capability().Wait()
				if err != nil {
					t.Fatalf("Capability() = %v", err)
				}
				assertWhitespaceCapabilities(t, caps)
				if err := client.Noop().Wait(); err != nil {
					t.Fatalf("Noop() after CAPABILITY = %v", err)
				}
				assertWhitespaceCapabilities(t, client.Caps())
			})
		}
	}
}

func TestCapabilityResponseCodeTrailingSpaces(t *testing.T) {
	for _, spaces := range []int{0, 1, 2, 8} {
		t.Run(fmt.Sprint(spaces), func(t *testing.T) {
			code := "[CAPABILITY imap4rev1   IDLE  ACL rights=kxte" + strings.Repeat(" ", spaces) + "]"
			client := newCapabilityWhitespaceServer(t, "* OK "+code+" ready\r\n", func(tag, command string) string {
				// This tests the tagged response-code reader as well as the
				// greeting reader. Both must leave the closing bracket intact.
				return tag + " OK " + code + " completed\r\n"
			})
			if err := client.WaitGreeting(); err != nil {
				t.Fatalf("WaitGreeting() = %v", err)
			}
			assertWhitespaceCapabilities(t, client.Caps())
			for i := 0; i < 2; i++ {
				if err := client.Noop().Wait(); err != nil {
					t.Fatalf("Noop %d = %v", i, err)
				}
				assertWhitespaceCapabilities(t, client.Caps())
			}
		})
	}
}

func TestCapabilityTrailingSpacesWithLogin(t *testing.T) {
	client := newCapabilityWhitespaceServer(t, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready\r\n", func(tag, command string) string {
		if command == "LOGIN" {
			return "* CAPABILITY IMAP4rev1   IDLE  ACL rights=kxte        \r\n" + tag + " OK logged in\r\n"
		}
		if command != "NOOP" {
			t.Errorf("unexpected %s after capabilities were supplied with LOGIN", command)
		}
		return tag + " OK completed\r\n"
	})
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	assertWhitespaceCapabilities(t, client.Caps())
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() after LOGIN = %v", err)
	}
}

// The capability line must finish parsing on its own. The server sends no
// further octets until the client has published the new capabilities.
func TestCapabilityTrailingSpacesDoesNotWaitForNextResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	allowCompletion := make(chan struct{})
	release := sync.OnceFunc(func() { close(allowCompletion) })
	defer release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			return
		}
		reader := bufio.NewReader(serverConn)
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		if _, err := io.WriteString(serverConn, "* CAPABILITY IMAP4rev1   IDLE  ACL rights=kxte        \r\n"); err != nil {
			return
		}
		<-allowCompletion
		if _, err := io.WriteString(serverConn, tag+" OK completed\r\n"); err != nil {
			return
		}
		line, err = reader.ReadString('\n')
		if err == nil {
			tag, _, _ = strings.Cut(line, " ")
			io.WriteString(serverConn, tag+" OK noop\r\n")
		}
	}()
	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 5 * time.Second})
	t.Cleanup(func() {
		release()
		clientConn.Close()
		serverConn.Close()
		client.Close()
		<-done
	})
	if err := client.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	cmd := client.Capability()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for !client.Caps().Has(imap.CapIdle) {
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("CAPABILITY line waited for the server's next response")
		}
	}
	assertWhitespaceCapabilities(t, client.Caps())
	release()
	caps, err := cmd.Wait()
	if err != nil {
		t.Fatalf("Capability() = %v", err)
	}
	assertWhitespaceCapabilities(t, caps)
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() after CAPABILITY = %v", err)
	}
}

func TestCapabilityWhitespaceRejectsMalformedResponses(t *testing.T) {
	for _, response := range []string{
		"* CAPABILITY IMAP4rev1   \tIDLE\r\n",
		"* CAPABILITY IMAP4rev1   \"IDLE\"\r\n",
		"* CAPABILITY IMAP4rev1   )\r\n",
		"* CAPABILITY IMAP4rev1   ]\r\n",
		"* OK [CAPABILITY IMAP4rev1   \r\n",
		"* CAPABILITY IMAP4rev1   ",
		"* CAPABILITY IMAP4rev1   \r",
	} {
		t.Run(fmt.Sprintf("%q", response), func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: time.Second})
			t.Cleanup(func() { serverConn.Close(); client.Close() })
			go func() {
				defer serverConn.Close()
				io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
				if _, err := bufio.NewReader(serverConn).ReadString('\n'); err == nil {
					io.WriteString(serverConn, response)
				}
			}()
			if _, err := client.Capability().Wait(); err == nil {
				t.Fatal("Capability() succeeded for a malformed or truncated response")
			}
		})
	}
}

func newCapabilityWhitespaceServer(t *testing.T, greeting string, reply func(tag, command string) string) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, greeting); err != nil {
			return
		}
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				t.Errorf("malformed command %q", line)
				return
			}
			if _, err := io.WriteString(serverConn, reply(fields[0], fields[1])); err != nil {
				return
			}
		}
	}()
	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: time.Second})
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
		client.Close()
		<-done
	})
	return client
}

func assertWhitespaceCapabilities(t *testing.T, caps imap.CapSet) {
	t.Helper()
	want := []imap.Cap{imap.CapIMAP4rev1, imap.CapIdle, imap.CapACL, imap.Cap("RIGHTS=kxte")}
	if len(caps) != len(want) {
		t.Fatalf("Caps() = %v, want exactly %v", caps, want)
	}
	for _, cap := range want {
		if !caps.Has(cap) {
			t.Errorf("Caps() = %v, missing %v", caps, cap)
		}
	}
}
