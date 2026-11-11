package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap/imapclient"
)

// newMultiLiteralServer starts a scripted server which refuses LOGIN and
// APPEND ... CATENATE with a tagged NO in place of the continuation request
// for their first literal, and accepts a plain APPEND's literal. Every command
// line it reads, the literal data of an accepted APPEND aside, is sent on the
// returned channel.
func newMultiLiteralServer(t *testing.T, caps string) (*imapclient.Client, <-chan string) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	serverConn.SetDeadline(time.Now().Add(5 * time.Second))

	lines := make(chan string, 16)
	go func() {
		defer serverConn.Close()
		defer close(lines)
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY "+caps+"] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			lines <- line
			tag, cmd, _ := strings.Cut(line, " ")
			switch {
			case strings.HasPrefix(cmd, "LOGIN "):
				io.WriteString(serverConn, tag+" NO [AUTHENTICATIONFAILED] Not today\r\n")
			case strings.HasPrefix(cmd, "APPEND ") && strings.Contains(cmd, " CATENATE "):
				io.WriteString(serverConn, tag+" NO [TRYCREATE] No such mailbox\r\n")
			case strings.HasPrefix(cmd, "APPEND "):
				io.WriteString(serverConn, "+ Ready\r\n")
				if _, err := br.ReadString('\n'); err != nil { // the literal and CRLF
					return
				}
				io.WriteString(serverConn, tag+" OK APPEND completed\r\n")
			case strings.HasPrefix(cmd, "NOOP"):
				io.WriteString(serverConn, tag+" OK NOOP completed\r\n")
			default:
				io.WriteString(serverConn, tag+" BAD unexpected command\r\n")
			}
		}
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	return client, lines
}

// appendWithin runs a plain APPEND, which needs a continuation request, and
// fails the test if it does not complete in time.
func appendWithin(t *testing.T, client *imapclient.Client) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		cmd := client.Append("INBOX", 5, nil)
		io.WriteString(cmd, "hello")
		cmd.Close()
		_, err := cmd.Wait()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Append() after the refused command = %v, want OK", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Append() after the refused command never got its continuation request")
	}
}

// When a command's first synchronizing literal is refused, a later literal of
// the same command is never sent. It must not wait for a continuation request
// either: one left registered takes the "+" meant for the next command, which
// then waits for good. Issue #164.
func TestRefusedLiteralLeavesNoContinuationRequest(t *testing.T) {
	t.Run("LOGIN", func(t *testing.T) {
		client, _ := newMultiLiteralServer(t, "IMAP4rev1")
		// Neither name has a quoted form: two synchronizing literals.
		if err := client.Login("us\ner", "pa\nss").Wait(); err == nil {
			t.Fatal("Login() = nil, want the refusal")
		}
		appendWithin(t, client)
	})
	t.Run("CATENATE", func(t *testing.T) {
		client, _ := newMultiLiteralServer(t, "IMAP4rev1 CATENATE")
		cmd := client.AppendCatenate("Missing", nil)
		for _, part := range []string{"hello", "world"} {
			w := cmd.Text(int64(len(part)))
			io.WriteString(w, part)
			w.Close()
		}
		cmd.Close()
		if _, err := cmd.Wait(); err == nil {
			t.Fatal("AppendCatenate() = nil, want NO [TRYCREATE]")
		}
		appendWithin(t, client)
	})
}

// With LITERAL- a literal of 4096 octets or fewer is non-synchronizing. After
// a longer, synchronizing literal of the same command is refused, it is not
// sent either: its octets must not go out in front of the next command.
// Issue #164.
func TestRefusedLiteralLeaksNoLaterLiteral(t *testing.T) {
	long := strings.Repeat("x", 5000)
	for _, tc := range []struct {
		name string
		send func(client *imapclient.Client)
	}{
		{"LOGIN", func(client *imapclient.Client) {
			client.Login(long, "LEAK\n").Wait()
		}},
		{"CATENATE", func(client *imapclient.Client) {
			cmd := client.AppendCatenate("Missing", nil)
			for _, part := range []string{long, "LEAK!"} {
				w := cmd.Text(int64(len(part)))
				io.WriteString(w, part)
				w.Close()
			}
			cmd.Close()
			cmd.Wait()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, lines := newMultiLiteralServer(t, "IMAP4rev1 LITERAL- CATENATE")
			tc.send(client)
			<-lines // the refused command
			if err := client.Noop().Wait(); err != nil {
				t.Errorf("Noop() = %v", err)
			}
			if got := <-lines; got != "T2 NOOP\r\n" {
				t.Errorf("the line after the refused command is %q, want %q", got, "T2 NOOP\r\n")
			}
		})
	}
}
