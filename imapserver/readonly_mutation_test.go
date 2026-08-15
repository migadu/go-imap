package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const readOnlyTestMessage = "From: <a@example.org>\r\nSubject: hi\r\n\r\nbody\r\n"

// roConn is a minimal raw IMAP connection for these tests.
type roConn struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func (rc *roConn) send(s string) {
	rc.t.Helper()
	if _, err := rc.c.Write([]byte(s + "\r\n")); err != nil {
		rc.t.Fatalf("write %q: %v", s, err)
	}
}

// readUntilTag returns every line up to and including the tagged completion.
func (rc *roConn) readUntilTag(tag string) []string {
	rc.t.Helper()
	var lines []string
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			rc.t.Fatalf("read (waiting for %v): %v", tag, err)
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines
		}
	}
}

// TestReadOnlyMailboxRejectsMutations is a regression test for a mailbox
// selected with EXAMINE accepting commands that change it.
//
// RFC 9051 Section 6.3.2: "The EXAMINE command is identical to SELECT and
// returns the same output; however, the selected mailbox is identified as
// read-only. No changes to the permanent state of the mailbox, including
// per-user state, are permitted."
//
// The connection already tracked selectedReadOnly and used it to skip the
// implicit expunge on CLOSE/UNSELECT, but nothing consulted it when a client
// sent STORE or EXPUNGE itself. Those went straight through to the backend,
// which is exactly the ambiguity upstream describes: a backend cannot tell an
// implicit close-time expunge from one the client asked for, so it cannot
// answer correctly either.
//
// Upstream report: emersion/go-imap#672 by foxcpp.
func TestReadOnlyMailboxRejectsMutations(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "STORE", command: "STORE 1 +FLAGS (\\Seen)"},
		{name: "UID STORE", command: "UID STORE 1 +FLAGS (\\Seen)"},
		{name: "EXPUNGE", command: "EXPUNGE"},
		{name: "UID EXPUNGE", command: "UID EXPUNGE 1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := newReadOnlyTestConn(t)

			rc.send("a2 EXAMINE INBOX")
			resp := rc.readUntilTag("a2")
			if !strings.Contains(resp[len(resp)-1], "OK") {
				t.Fatalf("EXAMINE = %q, want OK", resp[len(resp)-1])
			}
			// Sanity: the mailbox really is read-only for this session.
			var sawReadOnly bool
			for _, line := range resp {
				if strings.Contains(line, "READ-ONLY") {
					sawReadOnly = true
				}
			}
			if !sawReadOnly {
				t.Fatalf("EXAMINE did not report READ-ONLY:\n\t%s", strings.Join(resp, "\n\t"))
			}

			rc.send("a3 " + tc.command)
			resp = rc.readUntilTag("a3")
			tagged := resp[len(resp)-1]
			if !strings.HasPrefix(tagged, "a3 NO") {
				t.Errorf("%v on a read-only mailbox = %q, want NO:\n\t%s", tc.name, tagged, strings.Join(resp, "\n\t"))
			}
		})
	}
}

// TestReadWriteMailboxAllowsMutations is the guard: the same commands must keep
// working on a mailbox selected with SELECT.
func TestReadWriteMailboxAllowsMutations(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "STORE", command: "STORE 1 +FLAGS (\\Seen)"},
		{name: "UID STORE", command: "UID STORE 1 +FLAGS (\\Seen)"},
		{name: "EXPUNGE", command: "EXPUNGE"},
		{name: "UID EXPUNGE", command: "UID EXPUNGE 1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := newReadOnlyTestConn(t)

			rc.send("a2 SELECT INBOX")
			rc.readUntilTag("a2")

			rc.send("a3 " + tc.command)
			resp := rc.readUntilTag("a3")
			tagged := resp[len(resp)-1]
			if !strings.HasPrefix(tagged, "a3 OK") {
				t.Errorf("%v on a writable mailbox = %q, want OK:\n\t%s", tc.name, tagged, strings.Join(resp, "\n\t"))
			}
		})
	}
}

// newReadOnlyTestConn brings up a server with a single-message INBOX and
// returns a logged-in connection.
func newReadOnlyTestConn(t *testing.T) *roConn {
	t.Helper()

	const (
		username = "user"
		password = "pass"
	)

	memUser := imapmemserver.NewUser(username, password)
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create(INBOX): %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	rc := &roConn{t: t, c: conn, br: bufio.NewReader(conn)}
	if _, err := rc.br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("read greeting: %v", err)
	}
	rc.send("x LOGIN " + username + " " + password)
	rc.readUntilTag("x")

	// One message to act on.
	rc.send("a1 APPEND INBOX {" + strconv.Itoa(len(readOnlyTestMessage)) + "}")
	line, err := rc.br.ReadString('\n')
	if err != nil {
		t.Fatalf("read continuation request: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("APPEND: got %q, want a continuation request", line)
	}
	if _, err := rc.c.Write([]byte(readOnlyTestMessage + "\r\n")); err != nil {
		t.Fatalf("write literal: %v", err)
	}
	rc.readUntilTag("a1")

	return rc
}

// TestReadOnlyMailboxCloseStillSucceeds guards the interaction between the
// read-only rejection and the implicit expunge.
//
// RFC 3501 Section 6.4.2: "No messages are removed, and no error is given, if
// the mailbox is selected by an EXAMINE command or is otherwise selected
// read-only." CLOSE must therefore still return OK on a read-only mailbox --
// it must not pick up the NO that a client-issued EXPUNGE now gets.
func TestReadOnlyMailboxCloseStillSucceeds(t *testing.T) {
	for _, name := range []string{"CLOSE", "UNSELECT"} {
		t.Run(name, func(t *testing.T) {
			rc := newReadOnlyTestConn(t)

			rc.send("a2 EXAMINE INBOX")
			rc.readUntilTag("a2")

			rc.send("a3 " + name)
			resp := rc.readUntilTag("a3")
			tagged := resp[len(resp)-1]
			if !strings.HasPrefix(tagged, "a3 OK") {
				t.Errorf("%v on a read-only mailbox = %q, want OK:\n\t%s", name, tagged, strings.Join(resp, "\n\t"))
			}
		})
	}
}
