package imapclient_test

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestAppend(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	body := "This is a test message."

	appendCmd := client.Append("INBOX", int64(len(body)), nil)
	if _, err := appendCmd.Write([]byte(body)); err != nil {
		t.Fatalf("AppendCommand.Write() = %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("AppendCommand.Close() = %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("AppendCommand.Wait() = %v", err)
	}

	// TODO: fetch back message and check body
}

// TestAppend_MissingCRLF verifies that the server responds with BAD (not a
// spurious OK) when an APPEND command is malformed: the literal payload is not
// followed by the required CRLF terminator.
//
// Before the fix in imapserver/append.go, `if !dec.ExpectCRLF() { return err }`
// used the wrong variable `err` (stale nil from dec.ExpectLiteralReader) instead
// of `dec.Err()`, causing the server to silently ignore the missing CRLF and
// send "OK APPEND completed" for a malformed command.
func TestAppend_MissingCRLF(t *testing.T) {
	conn, server := newMemClientServerPair(t)
	defer server.Close()
	defer conn.Close()

	br := bufio.NewReader(conn)

	// Read greeting
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading greeting: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))

	// Login
	fmt.Fprintf(conn, "A1 LOGIN %s %s\r\n", testUsername, testPassword)
	// Drain all login response lines (capability OK may span multiple lines)
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading login response: %v", err)
		}
		t.Logf("S: %s", strings.TrimSpace(line))
		if strings.HasPrefix(line, "A1 ") {
			break
		}
	}
	if !strings.Contains(line, "A1 OK") {
		t.Fatalf("login failed: %s", strings.TrimSpace(line))
	}

	// Send APPEND with a synchronising literal (waits for + continuation)
	const literal = "hello"
	fmt.Fprintf(conn, "A2 APPEND INBOX {%d}\r\n", len(literal))

	// Read continuation request
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading continuation: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("expected continuation (+), got: %s", strings.TrimSpace(line))
	}

	// Send the literal ("hello", 5 bytes) then " GARBAGE\r\n" instead of the
	// required "\r\n".  The leading space is significant: dec.CRLF() consumes
	// an optional leading space before checking for \r\n, so the space is
	// consumed but then the parse fails on 'G' — triggering the bug path.
	//
	// APPEND uses sendOK=false and writes its own OK inside handleAppend.
	// With the bug (return err, where err==nil), handleAppend returns nil
	// without writing anything, and readCommand with sendOK=false also writes
	// nothing — the server just loops waiting for the next command.
	// With the fix (return dec.Err()), the server writes BAD.
	fmt.Fprintf(conn, "%s GARBAGE\r\n", literal)

	// Short deadline: the fix causes BAD to arrive quickly; the bug causes no
	// response and the read times out.
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	line, err = br.ReadString('\n')
	if err != nil {
		t.Errorf("Server sent no response for malformed APPEND (missing CRLF) — "+
			"this indicates the server silently ignored the parse error instead of returning BAD. err: %v", err)
		return
	}
	t.Logf("S: %s", strings.TrimSpace(line))

	if strings.Contains(line, "OK") && strings.Contains(line, "APPEND") {
		t.Errorf("server sent spurious OK for malformed APPEND (missing CRLF): %s", strings.TrimSpace(line))
	}
	if !strings.Contains(line, "BAD") {
		t.Errorf("expected BAD for malformed APPEND, got: %s", strings.TrimSpace(line))
	}
}
