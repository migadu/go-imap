package imapserver_test

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/migadu/go-imap/internal/imapwire"
)

func TestAppendCatenateDrainAggregateLimit(t *testing.T) {
	budget := int(imapwire.MaxDiscardLiteralSize)
	for _, tc := range []struct {
		name   string
		unauth bool
		parts  string
	}{
		{name: "unauthenticated URLs", unauth: true, parts: strings.Repeat("URL {4000+}\r\n"+strings.Repeat("u", 4000)+" ", budget/4000) + "URL {4000+}\r\n" + strings.Repeat("u", 4000)},
		{name: "TEXT then URL", parts: fmt.Sprintf("TEXT {%d+}\r\n", budget) + strings.Repeat("x", budget) + " URL {1+}\r\nu"},
		{name: "URLs then TEXT", parts: strings.Repeat("URL {4000+}\r\n"+strings.Repeat("u", 4000)+" ", budget/4000) + "TEXT {2000+}\r\n" + strings.Repeat("x", 2000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rc *searchRev2Conn
			mailbox := "Missing"
			if tc.unauth {
				rc = dialCatenateConn(t, 10)
				mailbox = "INBOX"
			} else {
				rc = newCatenateConn(t, 10)
			}
			command := "aggregate APPEND " + mailbox + " CATENATE (" + tc.parts + ")\r\n"
			go rc.c.Write([]byte(command))
			line, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("read BYE: %v", err)
			}
			if !strings.HasPrefix(line, "* BYE ") {
				t.Fatalf("aggregate literal payload exceeds %d bytes: got %q, want BYE", budget, line)
			}
		})
	}
}

func TestAppendCatenateDrainCommandPayload(t *testing.T) {
	payload := "injected CREATE injected-mailbox\r\n"
	for _, tc := range []struct {
		name, prefix, want string
		unauth             bool
	}{
		{name: "unauthenticated", prefix: "APPEND INBOX CATENATE (", want: " BAD ", unauth: true},
		{name: "backend BADURL", prefix: "APPEND INBOX CATENATE (URL \"/bad\" ", want: " NO [BADURL /bad] "},
		{name: "backend TRYCREATE", prefix: "APPEND Missing CATENATE (", want: " NO [TRYCREATE] "},
		{name: "TEXT too big", prefix: "APPEND INBOX CATENATE (", want: " NO [TOOBIG] "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rc *searchRev2Conn
			if tc.unauth {
				rc = dialCatenateConn(t, 10)
			} else {
				rc = newCatenateConn(t, 10)
			}
			command := tc.prefix + fmt.Sprintf("TEXT {%d+}\r\n", len(payload)) + payload + " URL {1+}\r\nu TEXT {3+}\r\nend)"
			resp, tagged := rc.try(command)
			if !strings.Contains(tagged, tc.want) {
				t.Fatalf("got %q, want %q", tagged, tc.want)
			}
			if strings.Contains(resp, "injected ") {
				t.Fatalf("literal payload was interpreted as a command: %q", resp)
			}
			rc.requireInSync("command-shaped TEXT payload")
			if tc.unauth {
				rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)
			}
			if resp := rc.do("LIST \"\" injected-mailbox"); strings.Contains(resp, "* LIST ") {
				t.Fatalf("literal payload created a mailbox: %q", resp)
			}
		})
	}
}

func TestAppendCatenateDrainMalformedLiteralStopsRecovery(t *testing.T) {
	rc := newCatenateConn(t, 10)
	payload := "injected CREATE injected-mailbox\r\n"
	command := fmt.Sprintf("malformed APPEND Missing CATENATE (TEXT {%d+}x\r\n%s)\r\nprobe NOOP\r\n", len(payload), payload)
	go rc.c.Write([]byte(command))
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return
			}
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, "* BYE ") {
			return
		}
		if strings.HasPrefix(line, "injected ") || strings.HasPrefix(line, "probe ") {
			t.Fatalf("malformed literal left a command executable: %q", line)
		}
	}
}

// Refusing a validated synchronizing literal does not consume its payload or
// end the connection: no continuation was sent, so the next command is clean.
func TestAppendCatenateDrainOverBudgetSynchronizing(t *testing.T) {
	rc := newCatenateConn(t, 10)
	tagged, continuations := rc.exchange("APPEND INBOX CATENATE (TEXT {65537}")
	if !strings.Contains(tagged, " NO [TOOBIG] ") || continuations != 0 {
		t.Fatalf("over-budget synchronizing literal: %q after %d continuations", tagged, continuations)
	}
	rc.requireInSync("over-budget synchronizing literal")
}

func TestAppendCatenateDrainAtAggregateLimit(t *testing.T) {
	rc := newCatenateConn(t, 10)
	payload := strings.Repeat("x", int(imapwire.MaxDiscardLiteralSize))
	_, tagged := rc.try(fmt.Sprintf("APPEND Missing CATENATE (TEXT {%d+}\r\n%s)", len(payload), payload))
	if !strings.Contains(tagged, " NO [TRYCREATE] ") {
		t.Fatalf("at-budget recovery changed the backend failure: %q", tagged)
	}
	rc.requireInSync("exact aggregate recovery budget")
}
