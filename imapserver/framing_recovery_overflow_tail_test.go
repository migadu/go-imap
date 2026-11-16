package imapserver_test

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWireContinuationLongLiteralSuffixClosesWithBye(t *testing.T) {
	for _, kind := range []string{"SASL", "IDLE"} {
		for _, countKind := range []string{"leading zeros", "overflow", "short suffix control"} {
			t.Run(kind+"/"+countKind, func(t *testing.T) {
				var w *parseWire
				if kind == "SASL" {
					w = dialFramingUnauthServer(t, "user1", "pass1")
					fmt.Fprint(w.conn, "a1 AUTHENTICATE PLAIN\r\n")
				} else {
					w = dialFramingIdleServer(t, nil)
					fmt.Fprint(w.conn, "a2 IDLE\r\n")
				}
				_ = w.conn.SetDeadline(time.Now().Add(3 * time.Second))
				if got := w.line(); !strings.HasPrefix(got, "+") {
					t.Fatalf("continuation = %q", got)
				}

				payload := "hidden CREATE injected-mailbox\r\n"
				if kind == "SASL" {
					payload = "hidden LOGIN user1 pass1\r\n"
				}
				count := fmt.Sprint(len(payload))
				prefixLen := 4000
				switch countKind {
				case "leading zeros":
					count = strings.Repeat("0", 200) + count
				case "overflow":
					count = strings.Repeat("9", 200)
				case "short suffix control":
					prefixLen = 5000
				}
				// The long digit run crosses a ReadSlice buffer boundary. Its
				// opening brace is well before the final chunk, but the complete
				// physical line still announces non-synchronizing literal data.
				wire := strings.Repeat("X", prefixLen) + "{" + count + "+}\r\n" + payload + "real NOOP\r\n"
				if _, err := io.WriteString(w.conn, wire); err != nil {
					t.Fatal(err)
				}
				_ = w.conn.(*net.TCPConn).CloseWrite()
				raw, err := io.ReadAll(w.rd)
				if err != nil {
					t.Fatalf("read: %v (output %q)", err, raw)
				}
				out := string(raw)
				if strings.Contains(out, "hidden ") || strings.Contains(out, "real ") {
					t.Fatalf("input after the rejected continuation's literal announcement executed as commands:\n%s", out)
				}
				if !strings.Contains(out, "* BYE ") {
					t.Fatalf("expected BYE for an unvalidated non-sync literal, got:\n%s", out)
				}
			})
		}
	}
}
