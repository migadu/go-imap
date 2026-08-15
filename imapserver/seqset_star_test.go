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

const starTestMessage = "From: <a@example.org>\r\nSubject: hi\r\n\r\nbody\r\n"

type rawConn struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func (rc *rawConn) send(s string) {
	rc.t.Helper()
	if _, err := rc.c.Write([]byte(s + "\r\n")); err != nil {
		rc.t.Fatalf("write %q: %v", s, err)
	}
}

// readUntilTag returns every line up to and including the tagged completion.
func (rc *rawConn) readUntilTag(tag string) []string {
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

func (rc *rawConn) appendMessage(tag, mailbox string) {
	rc.t.Helper()
	rc.send(tag + " APPEND " + mailbox + " {" + strconv.Itoa(len(starTestMessage)) + "}")
	line, err := rc.br.ReadString('\n')
	if err != nil {
		rc.t.Fatalf("read continuation request: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		rc.t.Fatalf("APPEND: got %q, want a continuation request", line)
	}
	if _, err := rc.c.Write([]byte(starTestMessage + "\r\n")); err != nil {
		rc.t.Fatalf("write literal: %v", err)
	}
	resp := rc.readUntilTag(tag)
	if !strings.Contains(resp[len(resp)-1], "OK") {
		rc.t.Fatalf("APPEND = %q, want OK", resp[len(resp)-1])
	}
}

// TestSeqSetStarUsesClientView is a regression test for '*' in a sequence set
// resolving against the server's message count instead of the client's.
//
// Expunges are held back from a session until it can safely receive them, so a
// session's view can legitimately contain MORE messages than the mailbox does.
// Resolving '*' against the server-side count then produces a range that is too
// short, and messages the client can still address are silently skipped:
// STORE 1:* quietly does nothing to them and reports OK.
//
// The reverse direction -- a message appended but not yet announced -- is
// already handled, because forEachLocked drops any message whose
// SessionTracker.EncodeSeqNum is 0. That guard cannot help here: these messages
// have perfectly good client sequence numbers, they just sit above the
// truncated range.
//
// RFC 9051 Section 2.3.1.2: '*' is "the largest number in use", a property of
// the client's view of the mailbox.
//
// Upstream: emersion/go-imap#586 by emersion.
func TestSeqSetStarUsesClientView(t *testing.T) {
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
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	dial := func() *rawConn {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("SetDeadline: %v", err)
		}
		rc := &rawConn{t: t, c: conn, br: bufio.NewReader(conn)}
		if _, err := rc.br.ReadString('\n'); err != nil { // greeting
			t.Fatalf("read greeting: %v", err)
		}
		rc.send("x LOGIN " + username + " " + password)
		rc.readUntilTag("x")
		return rc
	}

	a, b := dial(), dial()

	// Three messages, and A selects so its view is exactly those three.
	b.appendMessage("b1", "INBOX")
	b.appendMessage("b2", "INBOX")
	b.appendMessage("b3", "INBOX")
	a.send("a1 SELECT INBOX")
	a.readUntilTag("a1")

	// B removes the first one. A is not told: an expunge is withheld from a
	// session until it runs a command that can carry it, so A's view still has
	// three messages while the mailbox has two.
	b.send("b4 SELECT INBOX")
	b.readUntilTag("b4")
	b.send("b5 STORE 1 +FLAGS (\\Deleted)")
	b.readUntilTag("b5")
	b.send("b6 EXPUNGE")
	b.readUntilTag("b6")

	// A names '*' without having polled in between. In A's numbering the two
	// surviving messages are 2 and 3, so both must be flagged.
	a.send("a2 STORE 1:* +FLAGS (\\Seen)")
	resp := a.readUntilTag("a2")

	got := map[string]bool{}
	for _, line := range resp {
		for _, seqNum := range []string{"2", "3"} {
			if strings.HasPrefix(line, "* "+seqNum+" FETCH") && containsSeen(line) {
				got[seqNum] = true
			}
		}
	}
	for _, seqNum := range []string{"2", "3"} {
		if !got[seqNum] {
			t.Errorf("STORE 1:* did not touch message %v, which is inside the client's 1:* range:\n\t%s", seqNum, strings.Join(resp, "\n\t"))
		}
	}

	// Confirm against the mailbox rather than trusting the untagged responses:
	// after A catches up, both surviving messages must carry \Seen.
	a.send("a3 NOOP")
	a.readUntilTag("a3")

	a.send("a4 FETCH 1:* (FLAGS)")
	flags := a.readUntilTag("a4")
	var seen int
	for _, line := range flags {
		if strings.HasPrefix(line, "* ") && strings.Contains(line, "FETCH") && containsSeen(line) {
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("%v of 2 surviving messages carry \\Seen:\n\t%s", seen, strings.Join(flags, "\n\t"))
	}
}

// containsSeen reports whether a FETCH line carries the \Seen flag. Flag names
// are case-insensitive and this server lowercases them on the wire.
func containsSeen(line string) bool {
	return strings.Contains(strings.ToLower(line), "\\seen")
}
