package imapserver_test

import (
	"bufio"
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// TestUTF8MailboxNameNegotiation is an end-to-end regression test proving that a
// server advertising both IMAP4rev1 and IMAP4rev2 keeps mailbox names in
// Modified UTF-7 on the wire until the client sends ENABLE IMAP4rev2, then
// switches to UTF-8 (RFC 9051 Section 5.1, RFC 5161). Before the fix, UTF-8 was
// used as soon as IMAP4rev2 was advertised, breaking legacy clients.
func TestUTF8MailboxNameNegotiation(t *testing.T) {
	const (
		username = "user"
		password = "pass"
		// "Café" — differs between the two encodings.
		mailboxName = "Café"
		// Modified UTF-7 of the above: '&' shift, base64("\x00\xe9")="AOk", '-'.
		mailboxModUTF7 = "Caf&AOk-"
	)
	utf8Bytes := []byte(mailboxName)    // raw UTF-8, contains 0xC3 0xA9
	utf7Bytes := []byte(mailboxModUTF7) // pure ASCII

	memUser := imapmemserver.NewUser(username, password)
	if err := memUser.Create(mailboxName, nil); err != nil {
		t.Fatalf("Create(%q): %v", mailboxName, err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	// Dual-stack: advertise BOTH IMAP4rev1 and IMAP4rev2.
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
		InsecureAuth: true,
	})
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	br := bufio.NewReader(conn)

	send := func(s string) {
		if _, err := conn.Write([]byte(s + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}

	// readUntilTag accumulates response lines (incl. untagged data) until the
	// tagged completion line for tag. The mailbox names in this test always
	// encode as quoted strings (never literals), so line-based reading is safe.
	readUntilTag := func(tag string) []byte {
		var buf bytes.Buffer
		for {
			line, err := br.ReadString('\n')
			buf.WriteString(line)
			if strings.HasPrefix(line, tag+" ") {
				return buf.Bytes()
			}
			if err != nil {
				t.Fatalf("reading response for tag %q: %v (got %q)", tag, err, buf.String())
			}
		}
	}

	// Greeting.
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read greeting: %v", err)
	}

	send("a LOGIN " + username + " " + password)
	if resp := readUntilTag("a"); !bytes.Contains(resp, []byte("a OK")) {
		t.Fatalf("LOGIN failed: %q", resp)
	}

	// Before ENABLE: a dual-stack server MUST speak Modified UTF-7.
	send(`b LIST "" "*"`)
	resp := readUntilTag("b")
	if !bytes.Contains(resp, utf7Bytes) {
		t.Errorf("pre-ENABLE LIST: missing Modified UTF-7 name %q\nresponse: %q", mailboxModUTF7, resp)
	}
	if bytes.Contains(resp, utf8Bytes) {
		t.Errorf("pre-ENABLE LIST: leaked raw UTF-8 mailbox name to a non-rev2 client\nresponse: %q", resp)
	}

	send("c ENABLE IMAP4rev2")
	if resp := readUntilTag("c"); !bytes.Contains(resp, []byte("c OK")) {
		t.Fatalf("ENABLE IMAP4rev2 failed: %q", resp)
	}

	// After ENABLE: the server MUST switch to UTF-8.
	send(`d LIST "" "*"`)
	resp = readUntilTag("d")
	if !bytes.Contains(resp, utf8Bytes) {
		t.Errorf("post-ENABLE LIST: missing UTF-8 name %q\nresponse: %q", mailboxName, resp)
	}
	if bytes.Contains(resp, utf7Bytes) {
		t.Errorf("post-ENABLE LIST: still using Modified UTF-7 after ENABLE IMAP4rev2\nresponse: %q", resp)
	}

	send("z LOGOUT")
	readUntilTag("z")
}
