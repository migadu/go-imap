package imapserver_test

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// notifyTestConn is a raw-wire IMAP connection for exercising the NOTIFY
// command syntax without an IMAP client library in the way.
type notifyTestConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

func newNotifyTestServer(t *testing.T) (addr string, user *imapmemserver.User, closer func()) {
	t.Helper()

	user = imapmemserver.NewUser("user", "pass")
	memServer := imapmemserver.New()
	memServer.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapNotify:    {},
		},
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	return ln.Addr().String(), user, func() { srv.Close() }
}

func dialNotifyTest(t *testing.T, addr string) *notifyTestConn {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	c := &notifyTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("reading greeting: %v", err)
	}
	return c
}

func (c *notifyTestConn) close() {
	c.conn.Close()
}

// cmd sends an IMAP command and returns every line up to and including the
// tagged response.
func (c *notifyTestConn) cmd(format string, args ...interface{}) string {
	c.t.Helper()

	c.tag++
	tag := fmt.Sprintf("T%d", c.tag)
	line := tag + " " + fmt.Sprintf(format, args...)
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatalf("write %q: %v", line, err)
	}

	var buf bytes.Buffer
	for {
		l, err := c.br.ReadString('\n')
		buf.WriteString(l)
		if strings.HasPrefix(l, tag+" ") {
			return buf.String()
		}
		if err != nil {
			c.t.Fatalf("reading response for %q: %v (got %q)", line, err, buf.String())
		}
	}
}

// readLine reads one server line, waiting up to the given duration.
func (c *notifyTestConn) readLine(timeout time.Duration) (string, error) {
	c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return c.br.ReadString('\n')
}

func (c *notifyTestConn) login(t *testing.T) {
	t.Helper()
	if resp := c.cmd("LOGIN user pass"); !strings.Contains(resp, "OK") {
		t.Fatalf("LOGIN failed: %q", resp)
	}
}

func TestNotifyCommandSyntax(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	c := dialNotifyTest(t, addr)
	defer c.close()
	c.login(t)
	if resp := c.cmd("CREATE INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	if resp := c.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("SELECT failed: %q", resp)
	}

	tests := []struct {
		name string
		cmd  string
		want string // substring expected in the tagged response line
	}{
		{
			name: "None",
			cmd:  "NOTIFY NONE",
			want: "OK",
		},
		{
			name: "SetSelected",
			cmd:  "NOTIFY SET (SELECTED (MessageNew MessageExpunge))",
			want: "OK",
		},
		{
			name: "CaseInsensitive",
			cmd:  "notify set (selected-delayed (messagenew messageexpunge flagchange))",
			want: "OK",
		},
		{
			name: "EventsNone",
			cmd:  "NOTIFY SET (PERSONAL NONE)",
			want: "OK",
		},
		{
			name: "SubtreeBareMailbox",
			cmd:  "NOTIFY SET (SUBTREE INBOX (MailboxName))",
			want: "OK",
		},
		{
			name: "MailboxesList",
			cmd:  `NOTIFY SET (MAILBOXES (INBOX "Foo Bar") (MessageNew MessageExpunge))`,
			want: "OK",
		},
		{
			name: "SelectedFetchAtts",
			cmd:  "NOTIFY SET (SELECTED (MessageNew (UID FLAGS) MessageExpunge))",
			want: "OK",
		},
		{
			name: "MessageNewWithoutExpunge",
			cmd:  "NOTIFY SET (SELECTED (MessageNew))",
			want: "BAD",
		},
		{
			name: "MessageExpungeWithoutNew",
			cmd:  "NOTIFY SET (SELECTED (MessageExpunge))",
			want: "BAD",
		},
		{
			name: "FlagChangeWithoutMessagePair",
			cmd:  "NOTIFY SET (SELECTED (FlagChange))",
			want: "BAD",
		},
		{
			name: "TwoSelectedGroups",
			cmd:  "NOTIFY SET (SELECTED (MessageNew MessageExpunge)) (SELECTED-DELAYED (MessageNew MessageExpunge))",
			want: "BAD",
		},
		{
			name: "MailboxEventOnSelected",
			cmd:  "NOTIFY SET (SELECTED (MessageNew MessageExpunge MailboxName))",
			want: "BAD",
		},
		{
			name: "FetchAttsOnNonSelected",
			cmd:  "NOTIFY SET (PERSONAL (MessageNew (UID) MessageExpunge))",
			want: "BAD",
		},
		{
			name: "FetchAttsAfterNonMessageNew",
			cmd:  "NOTIFY SET (SELECTED (MessageExpunge (UID) MessageNew))",
			want: "BAD",
		},
		{
			name: "UnknownSpecifier",
			cmd:  "NOTIFY SET (BOGUS (MessageNew MessageExpunge))",
			want: "BAD",
		},
		{
			name: "SetWithoutGroups",
			cmd:  "NOTIFY SET",
			want: "BAD",
		},
		{
			name: "UnknownVerb",
			cmd:  "NOTIFY BOGUS",
			want: "BAD",
		},
		{
			name: "UnsupportedEvent",
			cmd:  "NOTIFY SET (SELECTED (MessageNew MessageExpunge AnnotationChange))",
			want: "NO [BADEVENT (MessageNew MessageExpunge FlagChange MailboxName SubscriptionChange)]",
		},
		{
			name: "UnknownEvent",
			cmd:  "NOTIFY SET (PERSONAL (SomeVendorEvent))",
			want: "NO [BADEVENT",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := c.cmd("%s", tc.cmd)
			lines := strings.Split(strings.TrimRight(resp, "\r\n"), "\r\n")
			tagged := lines[len(lines)-1]
			if !strings.Contains(tagged, tc.want) {
				t.Errorf("%s: expected tagged response containing %q, got %q", tc.cmd, tc.want, tagged)
			}
		})
	}

	// The watch left over from the table runs must be cleanly removable.
	if resp := c.cmd("NOTIFY NONE"); !strings.Contains(resp, "OK") {
		t.Errorf("NOTIFY NONE failed: %q", resp)
	}
}

func TestNotifyNotAuthenticated(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	c := dialNotifyTest(t, addr)
	defer c.close()

	resp := c.cmd("NOTIFY NONE")
	if !strings.Contains(resp, "BAD") {
		t.Errorf("expected BAD for NOTIFY before authentication, got %q", resp)
	}
}

// TestNotifySetStatus verifies that NOTIFY SET STATUS sends the initial
// STATUS responses for matching non-selected mailboxes before the tagged OK
// (RFC 5465 section 3.1).
func TestNotifySetStatus(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	c := dialNotifyTest(t, addr)
	defer c.close()
	c.login(t)
	for _, name := range []string{"INBOX", "Archive"} {
		if resp := c.cmd("CREATE %s", name); !strings.Contains(resp, "OK") {
			t.Fatalf("CREATE %s failed: %q", name, resp)
		}
	}
	if resp := c.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("SELECT failed: %q", resp)
	}

	resp := c.cmd("NOTIFY SET STATUS (PERSONAL (MessageNew MessageExpunge))")
	lines := strings.Split(strings.TrimRight(resp, "\r\n"), "\r\n")
	tagged := lines[len(lines)-1]
	if !strings.Contains(tagged, "OK") {
		t.Fatalf("NOTIFY SET STATUS failed: %q", resp)
	}

	// The selected mailbox (INBOX) must not get a STATUS; Archive must, and
	// it must appear before the tagged OK by construction (it is part of the
	// response transcript).
	var sawArchive bool
	for _, line := range lines[:len(lines)-1] {
		if strings.HasPrefix(line, "* STATUS") {
			if strings.Contains(line, "Archive") {
				sawArchive = true
				for _, item := range []string{"MESSAGES", "UIDNEXT", "UIDVALIDITY"} {
					if !strings.Contains(line, item) {
						t.Errorf("STATUS response %q is missing %v", line, item)
					}
				}
			}
			if strings.Contains(line, "INBOX") {
				t.Errorf("unexpected STATUS for the selected mailbox: %q", line)
			}
		}
	}
	if !sawArchive {
		t.Errorf("expected an initial STATUS response for Archive, got %q", resp)
	}
}

// TestNotifyMailboxEventsDelivery verifies asynchronous LIST delivery for
// MailboxName/SubscriptionChange events triggered by another connection, and
// that NOTIFY NONE stops delivery.
func TestNotifyMailboxEventsDelivery(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxName SubscriptionChange))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	// CREATE on the other connection: the watcher gets an unsolicited LIST.
	if resp := other.cmd("CREATE Foo"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	line, err := watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for LIST after CREATE: %v", err)
	}
	if !strings.HasPrefix(line, "* LIST") || !strings.Contains(line, "Foo") {
		t.Errorf("expected unsolicited LIST for Foo, got %q", line)
	}

	// RENAME: LIST for the new name.
	if resp := other.cmd("RENAME Foo Bar"); !strings.Contains(resp, "OK") {
		t.Fatalf("RENAME failed: %q", resp)
	}
	line, err = watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for LIST after RENAME: %v", err)
	}
	if !strings.HasPrefix(line, "* LIST") || !strings.Contains(line, "Bar") {
		t.Errorf("expected unsolicited LIST for Bar, got %q", line)
	}

	// SUBSCRIBE: LIST with \Subscribed.
	if resp := other.cmd("SUBSCRIBE Bar"); !strings.Contains(resp, "OK") {
		t.Fatalf("SUBSCRIBE failed: %q", resp)
	}
	line, err = watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for LIST after SUBSCRIBE: %v", err)
	}
	if !strings.Contains(line, "\\Subscribed") || !strings.Contains(line, "Bar") {
		t.Errorf("expected unsolicited LIST with \\Subscribed for Bar, got %q", line)
	}

	// DELETE: LIST with \NonExistent.
	if resp := other.cmd("DELETE Bar"); !strings.Contains(resp, "OK") {
		t.Fatalf("DELETE failed: %q", resp)
	}
	line, err = watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for LIST after DELETE: %v", err)
	}
	if !strings.Contains(line, "\\NonExistent") || !strings.Contains(line, "Bar") {
		t.Errorf("expected unsolicited LIST with \\NonExistent for Bar, got %q", line)
	}

	// NOTIFY NONE: no more notifications.
	if resp := watcher.cmd("NOTIFY NONE"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY NONE failed: %q", resp)
	}
	if resp := other.cmd("CREATE Baz"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	if line, err := watcher.readLine(300 * time.Millisecond); err == nil {
		t.Errorf("expected no notification after NOTIFY NONE, got %q", line)
	}
}
