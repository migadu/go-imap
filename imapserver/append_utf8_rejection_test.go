package imapserver_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap/imapserver"
)

func TestAppendUTF8RejectedLiteralKeepsConnection(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"plain control", "{5+}\r\nhello\r\n"},
		{"binary control", "~{5+}\r\nhello\r\n"},
		{"UTF8 wrapper", "UTF8 (~{5+}\r\nhello)\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
				return &appendSmallLimitSession{Session: session}
			})
			enableAppendUTF8(t, conn, br)
			if _, err := fmt.Fprintf(conn, "a1 APPEND INBOX %sa2 NOOP\r\n", tc.data); err != nil {
				t.Fatal(err)
			}
			line, err := br.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "a1 NO [TOOBIG]") {
				t.Fatalf("well-framed oversized APPEND = %q, %v; want tagged TOOBIG rejection", line, err)
			}
			line, err = br.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "a2 OK") {
				t.Fatalf("NOOP after rejection = %q, %v", line, err)
			}
		})
	}
}

func enableAppendUTF8(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	if _, err := io.WriteString(conn, "a0 ENABLE UTF8=ACCEPT\r\n"); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "a0 ") {
			if !strings.HasPrefix(line, "a0 OK") {
				t.Fatalf("ENABLE = %q", line)
			}
			return
		}
	}
}

func TestAppendUTF8BackendRefusalKeepsConnection(t *testing.T) {
	for _, nonSync := range []bool{false, true} {
		t.Run(fmt.Sprint(nonSync), func(t *testing.T) {
			conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
				return &appendRefusalSession{Session: session}
			})
			enableAppendUTF8(t, conn, br)
			modifier := ""
			if nonSync {
				modifier = "+"
			}
			if _, err := fmt.Fprintf(conn, "a1 APPEND INBOX UTF8 (~{5%s}\r\n", modifier); err != nil {
				t.Fatal(err)
			}
			if !nonSync {
				if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
					t.Fatalf("continuation = %q, %v", line, err)
				}
			}
			if _, err := io.WriteString(conn, "hello)\r\na2 NOOP\r\n"); err != nil {
				t.Fatal(err)
			}
			if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "a1 NO ") || strings.Contains(line, "SERVERBUG") {
				t.Fatalf("APPEND refusal = %q, %v", line, err)
			}
			if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "a2 OK") {
				t.Fatalf("pipelined NOOP = %q, %v", line, err)
			}
		})
	}
}
