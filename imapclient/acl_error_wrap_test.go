package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// A malformed ACL response fails the read loop, and the pending command
// returns that error. The decoder's error must stay reachable through the
// wrapping, so callers can use errors.As and errors.Is on it.
func TestACLResponseErrorsWrapDecoderError(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp string
		run  func(*imapclient.Client) error
	}{
		{"MYRIGHTS", "* MYRIGHTS INBOX\r\n", func(c *imapclient.Client) error {
			_, err := c.MyRights("INBOX").Wait()
			return err
		}},
		{"GETACL", "* ACL INBOX alice\r\n", func(c *imapclient.Client) error {
			_, err := c.GetACL("INBOX").Wait()
			return err
		}},
		{"LISTRIGHTS", "* LISTRIGHTS INBOX alice\r\n", func(c *imapclient.Client) error {
			_, err := c.ListRights("INBOX", "alice").Wait()
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

			go func() {
				defer serverConn.Close()
				io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ACL] ready\r\n")
				reader := bufio.NewReader(serverConn)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				tag := strings.Fields(line)[0]
				io.WriteString(serverConn, tc.resp)
				fmt.Fprintf(serverConn, "%s OK done\r\n", tag)
			}()

			client := imapclient.New(clientConn, nil)
			t.Cleanup(func() { client.Close() })
			if err := client.WaitGreeting(); err != nil {
				t.Fatalf("WaitGreeting() = %v", err)
			}

			err := tc.run(client)
			if err == nil {
				t.Fatal("malformed response accepted")
			}
			var expectErr *imapwire.DecoderExpectError
			if !errors.As(err, &expectErr) {
				t.Fatalf("errors.As(%q, *imapwire.DecoderExpectError) = false; the decoder error was not wrapped", err)
			}
		})
	}
}
