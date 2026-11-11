package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// appendWire runs one APPEND of body against a scripted server offering caps,
// and returns the command line the client sent ahead of the literal.
func appendWire(t *testing.T, caps, body string, options *imap.AppendOptions) string {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	lineCh := make(chan string, 1)
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY "+caps+"] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		lineCh <- line
		// A literal announced with "+}" needs no continuation request.
		if len(line) < 4 || line[len(line)-4:] != "+}\r\n" {
			io.WriteString(serverConn, "+ go ahead\r\n")
		}
		if _, err := io.ReadFull(br, make([]byte, len(body)+len("\r\n"))); err != nil {
			return
		}
		io.WriteString(serverConn, "T1 OK APPEND completed\r\n")
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })

	cmd := client.Append("INBOX", int64(len(body)), options)
	if _, err := io.WriteString(cmd, body); err != nil {
		t.Fatalf("AppendCommand.Write() = %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("AppendCommand.Close() = %v", err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("AppendCommand.Wait() = %v", err)
	}
	return <-lineCh
}

// RFC 3516 §4.4: "The APPEND command is extended to allow the client to
// append data containing NULs by using the <literal8> syntax."
//
//	append   =/ "APPEND" SP mailbox [SP flag-list] [SP date-time] SP literal8
//	literal8 =  "~{" number "}" CRLF *OCTET
//
// RFC 7888 §8 lets a literal8 be non-synchronizing as well:
//
//	literal8 = "~{" number ["+"] "}" CRLF *OCTET
//
// Upstream request: emersion/go-imap#670.
func TestAppendBinary(t *testing.T) {
	const body = "Subject: x\r\n\r\n\x00\x01\x02"
	for _, tc := range []struct {
		name    string
		caps    string
		options *imap.AppendOptions
		want    string
	}{
		{
			name:    "literal8",
			caps:    "IMAP4rev1 BINARY",
			options: &imap.AppendOptions{Binary: true},
			want:    "T1 APPEND INBOX ~{17}\r\n",
		},
		{
			name:    "non-synchronizing literal8",
			caps:    "IMAP4rev1 BINARY LITERAL-",
			options: &imap.AppendOptions{Binary: true},
			want:    "T1 APPEND INBOX ~{17+}\r\n",
		},
		{
			name:    "after the flags",
			caps:    "IMAP4rev1 BINARY",
			options: &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Binary: true},
			want:    "T1 APPEND INBOX (\\Seen) ~{17}\r\n",
		},
		{
			name: "a plain literal without the option",
			caps: "IMAP4rev1 BINARY",
			want: "T1 APPEND INBOX {17}\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := appendWire(t, tc.caps, body, tc.options); got != tc.want {
				t.Errorf("client sent %q, want %q", got, tc.want)
			}
		})
	}
}

// The client and the server agree on the literal8 form end to end.
func TestAppendBinaryRoundTrip(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapBinary) {
		t.Skip("server does not offer BINARY")
	}

	const body = "Subject: x\r\n\r\n\x00\x01\x02"
	cmd := client.Append("INBOX", int64(len(body)), &imap.AppendOptions{Binary: true})
	if _, err := io.WriteString(cmd, body); err != nil {
		t.Fatalf("AppendCommand.Write() = %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("AppendCommand.Close() = %v", err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("AppendCommand.Wait() = %v", err)
	}
}
