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

func TestAppend_UnauthenticatedSyncLiteral(t *testing.T) {
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

	// Send APPEND before LOGIN with a synchronising literal
	fmt.Fprintf(conn, "A1 APPEND INBOX {10}\r\n")

	// The server should NOT send a continuation request "+" because the client is not authenticated.
	// Instead, it should directly send a status response (NO or BAD) for A1.
	conn.SetDeadline(time.Now().Add(1 * time.Second))
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading response to unauthenticated APPEND: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.HasPrefix(line, "A1 ") {
		t.Fatalf("expected status response for A1, got: %s", strings.TrimSpace(line))
	}
	if strings.Contains(line, "OK") {
		t.Fatalf("unauthenticated APPEND should not succeed, got: %s", strings.TrimSpace(line))
	}

	// Now send A2 NOOP and verify it succeeds.
	fmt.Fprintf(conn, "A2 NOOP\r\n")
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading A2 NOOP response: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.Contains(line, "A2 OK") {
		t.Errorf("expected A2 OK, got: %s", strings.TrimSpace(line))
	}
}

func TestAppend_UnauthenticatedNonSyncLiteral(t *testing.T) {
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

	// Send APPEND before LOGIN with a non-synchronising literal. The literal
	// bytes are already on the wire, so the server must drain them before
	// rejecting the command, otherwise they leak into the command parser and
	// corrupt the next command.
	const literal = "0123456789"
	fmt.Fprintf(conn, "A1 APPEND INBOX {%d+}\r\n%s\r\n", len(literal), literal)

	// The server should reject A1 (not valid before authentication) without
	// sending a continuation request.
	conn.SetDeadline(time.Now().Add(1 * time.Second))
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading response to unauthenticated APPEND: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.HasPrefix(line, "A1 ") {
		t.Fatalf("expected status response for A1, got: %s", strings.TrimSpace(line))
	}
	if strings.Contains(line, "OK") {
		t.Fatalf("unauthenticated APPEND should not succeed, got: %s", strings.TrimSpace(line))
	}

	// Send A2 NOOP and verify it parses cleanly, proving the literal was drained.
	fmt.Fprintf(conn, "A2 NOOP\r\n")
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading A2 NOOP response: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.Contains(line, "A2 OK") {
		t.Errorf("expected A2 OK, got: %s (this implies the literal was not drained)", strings.TrimSpace(line))
	}
}

func TestAppend_BareLiteral8(t *testing.T) {
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

	// Send APPEND with bare literal8 (prefixed with ~)
	const literal = "hello binary"
	fmt.Fprintf(conn, "A2 APPEND INBOX ~{%d}\r\n", len(literal))

	// Read continuation "+" request
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading continuation: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("expected continuation (+), got: %s", strings.TrimSpace(line))
	}

	// Write literal data, then CRLF
	fmt.Fprintf(conn, "%s\r\n", literal)

	// Read response
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading APPEND response: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.Contains(line, "A2 OK") {
		t.Errorf("expected A2 OK, got: %s", strings.TrimSpace(line))
	}
}

// TestAppend_UTF8 verifies the RFC 6855 "UTF8 (~{n}...)" APPEND data extension.
// The keyword is case-insensitive, so both "UTF8" and "utf8" must be accepted;
// the lowercase case is a regression guard for a bug where the closing ')' was
// only consumed when the keyword matched "UTF8" exactly.
func TestAppend_UTF8(t *testing.T) {
	for _, keyword := range []string{"UTF8", "utf8"} {
		t.Run(keyword, func(t *testing.T) {
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

			// Send APPEND with the UTF8 data extension: UTF8 (~{n}...)
			const literal = "hello utf8"
			fmt.Fprintf(conn, "A2 APPEND INBOX %s (~{%d}\r\n", keyword, len(literal))

			// Read continuation "+" request
			line, err = br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading continuation: %v", err)
			}
			t.Logf("S: %s", strings.TrimSpace(line))
			if !strings.HasPrefix(line, "+") {
				t.Fatalf("expected continuation (+), got: %s", strings.TrimSpace(line))
			}

			// Write literal data, the closing ')', then CRLF
			fmt.Fprintf(conn, "%s)\r\n", literal)

			// Read response
			line, err = br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading APPEND response: %v", err)
			}
			t.Logf("S: %s", strings.TrimSpace(line))
			if !strings.Contains(line, "A2 OK") {
				t.Errorf("expected A2 OK, got: %s", strings.TrimSpace(line))
			}
		})
	}
}

func TestAppend_NonSyncLiteralRejectDraining(t *testing.T) {
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

	// Send APPEND with oversized non-synchronising literal (4097 bytes) when LITERAL+ is not supported
	payload := strings.Repeat("A", 4097)
	fmt.Fprintf(conn, "A2 APPEND INBOX {%d+}\r\n%s\r\n", len(payload), payload)

	// Send subsequent command on the same stream
	fmt.Fprintf(conn, "A3 NOOP\r\n")

	// Read A2 response
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading A2 response: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.Contains(line, "A2 BAD") {
		t.Errorf("expected A2 BAD, got: %s", strings.TrimSpace(line))
	}

	// Read A3 response
	line, err = br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading A3 response: %v", err)
	}
	t.Logf("S: %s", strings.TrimSpace(line))
	if !strings.Contains(line, "A3 OK") {
		t.Errorf("expected A3 OK, got: %s (this implies the server command stream got corrupted)", strings.TrimSpace(line))
	}
}
