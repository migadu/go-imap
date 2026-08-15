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

// TestAppendTruncatedLiteralIsRejected is the server-side half of the truncated
// literal regression.
//
// A client announces APPEND {N} and then disappears after fewer than N bytes.
// The literal reader used to report the underlying io.EOF verbatim, so the
// backend's io.Copy returned successfully and the short payload was stored as a
// complete message. The client never saw a tagged OK -- the connection was gone
// -- so from its point of view the APPEND failed, while the mailbox had gained
// a silently truncated copy.
//
// Upstream report: emersion/go-imap#650, fix proposed in emersion/go-imap#676
// by vzeroupper.
func TestAppendTruncatedLiteralIsRejected(t *testing.T) {
	const (
		username = "user"
		password = "pass"
		// Announced size is deliberately larger than what we send.
		announcedSize = 200
	)
	partial := "From: <a@example.org>\r\nSubject: truncated\r\n\r\nhalf a mes"

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

	dial := func() (net.Conn, *bufio.Reader) {
		t.Helper()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("SetDeadline: %v", err)
		}
		br := bufio.NewReader(conn)
		if _, err := br.ReadString('\n'); err != nil { // greeting
			t.Fatalf("read greeting: %v", err)
		}
		return conn, br
	}

	readUntilTag := func(br *bufio.Reader, tag string) string {
		t.Helper()
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			sb.WriteString(line)
			if strings.HasPrefix(line, tag+" ") {
				return sb.String()
			}
		}
	}

	// Announce a 200-byte literal, then send a short prefix and hang up.
	conn, br := dial()
	if _, err := conn.Write([]byte("a1 LOGIN " + username + " " + password + "\r\n")); err != nil {
		t.Fatalf("write LOGIN: %v", err)
	}
	readUntilTag(br, "a1")

	if _, err := conn.Write([]byte("a2 APPEND INBOX {" + strconv.Itoa(announcedSize) + "}\r\n")); err != nil {
		t.Fatalf("write APPEND: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read continuation request: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("APPEND: got %q, want a continuation request", line)
	}
	if _, err := conn.Write([]byte(partial)); err != nil {
		t.Fatalf("write partial literal: %v", err)
	}
	conn.Close()

	// The server is still processing the half-delivered APPEND. Watch the
	// mailbox for long enough that it has certainly finished and torn the
	// connection down, failing as soon as a message shows up rather than
	// sampling once and racing the server.
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := mailboxMessageCount(t, memUser); n != 0 {
			t.Fatalf("INBOX gained %d message(s) from a truncated APPEND: the server stored %d of the %d announced bytes as a complete message", n, len(partial), announcedSize)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if n := mailboxMessageCount(t, memUser); n != 0 {
		t.Fatalf("INBOX has %d message(s) after a truncated APPEND, want 0: the server stored %d of the %d announced bytes as a complete message", n, len(partial), announcedSize)
	}
}

// mailboxMessageCount reports how many messages INBOX holds, via a fresh
// session so it never observes the torn-down connection's state.
func mailboxMessageCount(t *testing.T, u *imapmemserver.User) int {
	t.Helper()
	data, err := u.Status(context.Background(), "INBOX", &imap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatalf("Status(INBOX): %v", err)
	}
	if data.NumMessages == nil {
		t.Fatalf("Status(INBOX): no NumMessages")
	}
	return int(*data.NumMessages)
}
