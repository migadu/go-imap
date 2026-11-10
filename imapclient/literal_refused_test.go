package imapclient_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// A server may answer a command instead of asking for its synchronizing
// literal; for APPEND that is how it says the mailbox does not exist or the
// message is too large. RFC 9051 §4.3: "the client MUST wait to receive a
// command continuation request [...] before sending the octet data (and the
// remainder of the command)". With the tagged response in its place there is
// nothing left to send, and the command line already ended in CRLF: the next
// command starts on a clean line.
//
// The client used to close the connection when the refused command was
// closed. Issue #126.
func TestAppendRefusedBeforeLiteral(t *testing.T) {
	client := newRefusingServer(t)

	for i := 0; i < 2; i++ {
		cmd := client.Append("Missing", 5, nil)
		// The refusal reaches the writer; a caller may well ignore it and
		// only look at Wait.
		io.WriteString(cmd, "hello")
		cmd.Close()
		_, err := cmd.Wait()
		var imapErr *imap.Error
		if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeTryCreate {
			t.Fatalf("Append().Wait() = %v, want NO [TRYCREATE]", err)
		}

		if err := client.Noop().Wait(); err != nil {
			t.Fatalf("Noop() after a refused APPEND = %v, want the connection still usable", err)
		}
	}
}

// The same holds for a literal the encoder writes by itself, such as a user
// name that has no quoted form.
func TestLoginRefusedBeforeLiteral(t *testing.T) {
	client := newRefusingServer(t)

	err := client.Login("us\ner", "pass").Wait()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeAuthenticationFailed {
		t.Fatalf("Login().Wait() = %v, want NO [AUTHENTICATIONFAILED]", err)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() after a refused LOGIN = %v, want the connection still usable", err)
	}
}

// newRefusingServer starts a scripted server which answers APPEND and LOGIN
// with a tagged NO as soon as it has read their first line, that is, in
// place of the continuation request for their literal.
func newRefusingServer(t *testing.T) *imapclient.Client {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				// Octets of a literal nobody asked for: fail the command
				// that follows, as a server would.
				io.WriteString(serverConn, "* BAD unexpected data\r\n")
				continue
			}
			switch fields[1] {
			case "APPEND":
				io.WriteString(serverConn, fields[0]+" NO [TRYCREATE] No such mailbox\r\n")
			case "LOGIN":
				io.WriteString(serverConn, fields[0]+" NO [AUTHENTICATIONFAILED] Not today\r\n")
			case "NOOP":
				io.WriteString(serverConn, fields[0]+" OK NOOP completed\r\n")
			default:
				io.WriteString(serverConn, fields[0]+" BAD unexpected command\r\n")
			}
		}
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	return client
}
