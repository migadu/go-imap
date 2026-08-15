package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
)

// newCodeServer starts a server that answers every command with respLines
// followed by a tagged OK, and returns a connected client.
func newCodeServer(t *testing.T, respLines string) *imapclient.Client {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			io.WriteString(serverConn, respLines)
			io.WriteString(serverConn, fields[0]+" OK completed\r\n")
		}
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	return client
}

// TestMalformedRespTextCodeDoesNotKillConnection is a regression test for a
// response code whose argument does not fit its type taking down the whole
// connection.
//
// A resp-text-code is advisory metadata attached to a status response. RFC 9051
// Section 7.1 already requires a client to ignore codes it does not recognise,
// so failing the connection over one it recognises but cannot parse is a much
// harsher reaction than the protocol asks for.
//
// This is not hypothetical: dynadot sends a millisecond timestamp as
// UIDVALIDITY, which does not fit the 32 bits RFC 9051 gives it. The client
// could not talk to that server at all -- the first command failed with
//
//	in response-data: imapwire: expected number, got "]"
//
// and every command after it with "use of closed network connection".
//
// Upstream report: emersion/go-imap#612 by quzhi1.
func TestMalformedRespTextCodeDoesNotKillConnection(t *testing.T) {
	tests := []struct {
		name string
		resp string
	}{
		{
			// The reported case: 1713632002544 > 2^32.
			name: "UIDVALIDITY wider than 32 bits",
			resp: "* OK [UIDVALIDITY 1713632002544] UIDs valid\r\n",
		},
		{
			name: "UIDNEXT wider than 32 bits",
			resp: "* OK [UIDNEXT 99999999999999] Predicted next UID\r\n",
		},
		{
			name: "HIGHESTMODSEQ wider than 64 bits",
			resp: "* OK [HIGHESTMODSEQ 184467440737095516160] Highest\r\n",
		},
		{
			name: "UIDVALIDITY with no argument",
			resp: "* OK [UIDVALIDITY] UIDs valid\r\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newCodeServer(t, tc.resp)

			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop().Wait() = %v, want the malformed code to be ignored", err)
			}
			// The connection must still be usable afterwards.
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("second Noop().Wait() = %v, want the connection to survive", err)
			}
		})
	}
}

// TestWellFormedRespTextCodeStillParsed is the guard for the change from the
// Expect parsers to the tolerant ones: a well-formed code must still be read
// and its value delivered, not quietly skipped.
func TestWellFormedRespTextCodeStillParsed(t *testing.T) {
	client := newCodeServer(t, "* OK [UIDVALIDITY 3857529045] UIDs valid\r\n"+
		"* OK [UIDNEXT 4392] Predicted next UID\r\n"+
		"* OK [HIGHESTMODSEQ 90060128194045800] Highest\r\n")

	data, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("Select().Wait() = %v", err)
	}
	if data.UIDValidity != 3857529045 {
		t.Errorf("UIDValidity = %v, want 3857529045", data.UIDValidity)
	}
	if data.UIDNext != 4392 {
		t.Errorf("UIDNext = %v, want 4392", data.UIDNext)
	}
	if data.HighestModSeq != 90060128194045800 {
		t.Errorf("HighestModSeq = %v, want 90060128194045800", data.HighestModSeq)
	}
}

// TestMalformedRespTextCodeKeepsStatusResponse checks that skipping a code does
// not swallow the status response carrying it: a tagged NO must still be an
// error, and its text must survive.
func TestMalformedRespTextCodeKeepsStatusResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			io.WriteString(serverConn, fields[0]+" NO [UIDVALIDITY 1713632002544] mailbox is broken\r\n")
		}
	}()

	client := imapclient.New(clientConn, nil)
	defer client.Close()
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	err := client.Noop().Wait()
	if err == nil {
		t.Fatal("Noop().Wait() = nil, want the tagged NO to surface as an error")
	}
	if !strings.Contains(err.Error(), "mailbox is broken") {
		t.Errorf("Noop().Wait() = %v, want the server text preserved", err)
	}
}

// appendUIDServer answers an APPEND with the given tagged completion line.
func appendUIDServer(t *testing.T, taggedLine string) *imapclient.Client {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if strings.EqualFold(fields[1], "APPEND") {
				// IMAP4rev2 implies non-synchronizing literals, so the client
				// does not wait for a continuation request: read exactly the
				// announced number of bytes, then the CRLF after them.
				open := strings.LastIndex(line, "{")
				close := strings.LastIndex(line, "}")
				if open < 0 || close < open {
					return
				}
				size, err := strconv.Atoi(strings.TrimSuffix(line[open+1:close], "+"))
				if err != nil {
					return
				}
				if _, err := io.ReadFull(br, make([]byte, size)); err != nil {
					return
				}
				if _, err := br.ReadString('\n'); err != nil {
					return
				}
				io.WriteString(serverConn, fields[0]+" "+taggedLine+"\r\n")
				continue
			}
			io.WriteString(serverConn, fields[0]+" OK completed\r\n")
		}
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	return client
}

// TestMalformedAppendUIDDoesNotKillConnection covers the tagged-response code
// parser, which is a separate switch from the untagged one. APPENDUID carries
// a UIDVALIDITY too, so the same server that overflows it on SELECT overflows
// it here.
func TestMalformedAppendUIDDoesNotKillConnection(t *testing.T) {
	const msg = "From: <a@example.org>\r\n\r\nbody\r\n"

	client := appendUIDServer(t, "OK [APPENDUID 1713632002544 5] APPEND completed")

	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	appendCmd.Write([]byte(msg))
	appendCmd.Close()
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("Append().Wait() = %v, want the malformed APPENDUID to be ignored", err)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop().Wait() = %v, want the connection to survive", err)
	}
}

// TestWellFormedAppendUIDStillParsed is the matching guard.
func TestWellFormedAppendUIDStillParsed(t *testing.T) {
	const msg = "From: <a@example.org>\r\n\r\nbody\r\n"

	client := appendUIDServer(t, "OK [APPENDUID 3857529045 5] APPEND completed")

	appendCmd := client.Append("INBOX", int64(len(msg)), nil)
	appendCmd.Write([]byte(msg))
	appendCmd.Close()
	data, err := appendCmd.Wait()
	if err != nil {
		t.Fatalf("Append().Wait() = %v", err)
	}
	if data.UIDValidity != 3857529045 {
		t.Errorf("UIDValidity = %v, want 3857529045", data.UIDValidity)
	}
	if data.UID != 5 {
		t.Errorf("UID = %v, want 5", data.UID)
	}
}
