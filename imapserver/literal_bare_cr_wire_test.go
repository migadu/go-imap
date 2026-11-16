package imapserver_test

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Rejecting a literal does not make a bare CR into a command boundary. The
// CREATE after it is still part of the malformed physical line, and must never
// execute as a new command after the rejected SELECT.
func TestMalformedLiteralBareCRDoesNotExecuteSameLineCommand(t *testing.T) {
	for _, prefix := range []string{
		"{x}",
		"{3+}\r",
		"{3+} ",
		"{-1+}",
		"{3+x}",
	} {
		t.Run(prefix, func(t *testing.T) {
			w := dialParseServer(t, nil)
			_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := fmt.Fprintf(w.conn, "a2 SELECT %s\rhidden CREATE injected-mailbox\r\nreal NOOP\r\n", prefix); err != nil {
				t.Fatal(err)
			}
			_ = w.conn.(*net.TCPConn).CloseWrite()
			raw, err := io.ReadAll(w.rd)
			if err != nil {
				t.Fatalf("reading connection close: %v (output %q)", err, raw)
			}
			out := string(raw)
			if strings.Contains(out, "hidden ") {
				t.Fatalf("same-line CREATE was parsed as a new command:\n%s", out)
			}
			if !strings.Contains(out, "* BYE ") {
				t.Fatalf("unsafe recovery did not end the connection with BYE:\n%s", out)
			}
			if strings.Contains(out, "real ") {
				t.Fatalf("connection remained in service after unsafe recovery:\n%s", out)
			}
		})
	}
}

func TestMalformedLiteralCompleteLineAllowsFollowingCommand(t *testing.T) {
	for _, prefix := range []string{
		"{x}",
		"{3+}\r",
		"{3+} ",
		"{-1+}",
		"{3+x}",
	} {
		t.Run(prefix, func(t *testing.T) {
			w := dialParseServer(t, nil)
			_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := fmt.Fprintf(w.conn, "a2 SELECT %s\r\nreal NOOP\r\n", prefix); err != nil {
				t.Fatal(err)
			}
			_ = w.conn.(*net.TCPConn).CloseWrite()
			raw, err := io.ReadAll(w.rd)
			if err != nil {
				t.Fatalf("reading command replies: %v (output %q)", err, raw)
			}
			out := string(raw)
			if !strings.Contains(out, "a2 BAD ") || !strings.Contains(out, "real OK ") || strings.Contains(out, "* BYE ") {
				t.Fatalf("complete malformed line did not recover with BAD followed by NOOP OK:\n%s", out)
			}
		})
	}
}
