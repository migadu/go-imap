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
			imap.CapMetadata:  {},
			imap.CapIdle:      {},
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
			want: "NO [BADEVENT (MessageNew MessageExpunge FlagChange MailboxName SubscriptionChange MailboxMetadataChange ServerMetadataChange)]",
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

// appendMessage appends a one-line message to the given mailbox. The literal
// is sent without waiting for the continuation request, which the server
// accepts.
func (c *notifyTestConn) appendMessage(t *testing.T, mailbox string) {
	t.Helper()
	if resp := c.cmd("APPEND %s {5}\r\nhello", mailbox); !strings.Contains(resp, "OK") {
		t.Fatalf("APPEND %s failed: %q", mailbox, resp)
	}
}

// TestNotifySelectedNoneSuppressesSelectedUpdates verifies RFC 5465 section
// 3.1: "If the SELECTED/SELECTED-DELAYED mailbox selector is not specified in
// the NOTIFY SET command, this means that the client doesn't want to receive
// any <message-event>s for the currently selected mailbox. This is the same as
// specifying SELECTED NONE."
//
// The suppression must also hold at command sync points: a NOOP must not
// report EXISTS for the selected mailbox.
func TestNotifySelectedNoneSuppressesSelectedUpdates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify string
	}{
		{name: "SelectedOmitted", notify: "NOTIFY SET (PERSONAL (MessageNew MessageExpunge))"},
		{name: "SelectedNone", notify: "NOTIFY SET (SELECTED NONE) (PERSONAL (MailboxName))"},
		{name: "NotifyNone", notify: "NOTIFY NONE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, closer := newNotifyTestServer(t)
			defer closer()

			watcher := dialNotifyTest(t, addr)
			defer watcher.close()
			watcher.login(t)
			if resp := watcher.cmd("CREATE INBOX"); !strings.Contains(resp, "OK") {
				t.Fatalf("CREATE failed: %q", resp)
			}
			if resp := watcher.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
				t.Fatalf("SELECT failed: %q", resp)
			}
			if resp := watcher.cmd("%s", tc.notify); !strings.Contains(resp, "OK") {
				t.Fatalf("%s failed: %q", tc.notify, resp)
			}

			other := dialNotifyTest(t, addr)
			defer other.close()
			other.login(t)
			other.appendMessage(t, "INBOX")

			// No asynchronous message event for the selected mailbox.
			if line, err := watcher.readLine(300 * time.Millisecond); err == nil {
				t.Errorf("unexpected unsolicited response for the selected mailbox: %q", line)
			}

			// And none at the next sync point either.
			resp := watcher.cmd("NOOP")
			if strings.Contains(resp, "EXISTS") {
				t.Errorf("expected no EXISTS for the selected mailbox at a sync point, got %q", resp)
			}
		})
	}
}

// TestNotifySelectedEventFiltering verifies that only the message events
// requested with the SELECTED specifier are reported for the selected mailbox
// (RFC 5465 sections 3.1 and 5): a watch without FlagChange must not produce
// unsolicited FETCH responses, while MessageNew must still be delivered.
func TestNotifySelectedEventFiltering(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	if resp := watcher.cmd("CREATE INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	watcher.appendMessage(t, "INBOX")
	if resp := watcher.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("SELECT failed: %q", resp)
	}
	if resp := watcher.cmd("NOTIFY SET (SELECTED (MessageNew MessageExpunge))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	if resp := other.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("SELECT failed: %q", resp)
	}
	if resp := other.cmd(`STORE 1 +FLAGS (\Seen)`); !strings.Contains(resp, "OK") {
		t.Fatalf("STORE failed: %q", resp)
	}

	// FlagChange was not requested: neither asynchronously...
	if line, err := watcher.readLine(300 * time.Millisecond); err == nil {
		t.Errorf("unexpected unsolicited response for an unrequested FlagChange: %q", line)
	}
	// ...nor at a sync point.
	if resp := watcher.cmd("NOOP"); strings.Contains(resp, "FETCH") {
		t.Errorf("expected no FETCH for an unrequested FlagChange, got %q", resp)
	}

	// MessageNew was requested, so it must still be delivered.
	other.appendMessage(t, "INBOX")
	line, err := watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for EXISTS after append: %v", err)
	}
	if !strings.Contains(line, "EXISTS") {
		t.Errorf("expected an unsolicited EXISTS, got %q", line)
	}
	if strings.Contains(line, "FETCH") {
		t.Errorf("expected no FETCH for an unrequested FlagChange, got %q", line)
	}
}

// TestNotifySelfCausedEvents verifies RFC 5465 section 5: "The server SHOULD
// omit notifying the client if the event is caused by this client."
func TestNotifySelfCausedEvents(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxName SubscriptionChange))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	// Caused by this client: no notification.
	if resp := watcher.cmd("CREATE SelfMade"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	if line, err := watcher.readLine(300 * time.Millisecond); err == nil {
		t.Errorf("unexpected notification for a self-caused event: %q", line)
	}
	if resp := watcher.cmd("SUBSCRIBE SelfMade"); !strings.Contains(resp, "OK") {
		t.Fatalf("SUBSCRIBE failed: %q", resp)
	}
	if line, err := watcher.readLine(300 * time.Millisecond); err == nil {
		t.Errorf("unexpected notification for a self-caused event: %q", line)
	}

	// Caused by another client: notification as usual.
	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	if resp := other.cmd("CREATE OtherMade"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	line, err := watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for LIST after foreign CREATE: %v", err)
	}
	if !strings.HasPrefix(line, "* LIST") || !strings.Contains(line, "OtherMade") {
		t.Errorf("expected an unsolicited LIST for OtherMade, got %q", line)
	}
}

// TestNotifyMetadataChangeDelivery verifies RFC 5465 sections 5.6 and 5.7:
// MailboxMetadataChange and ServerMetadataChange are reported with unsolicited
// METADATA responses. Support is REQUIRED when the server implements METADATA
// (RFC 5464). The responses carry entry names only — RFC 5464 section 4.4:
// "Unsolicited METADATA responses MUST only contain entry names, not the
// values."
func TestNotifyMetadataChangeDelivery(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	if resp := watcher.cmd("CREATE INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxMetadataChange ServerMetadataChange))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	if resp := other.cmd(`SETMETADATA INBOX (/private/comment "hi")`); !strings.Contains(resp, "OK") {
		t.Fatalf("SETMETADATA failed: %q", resp)
	}
	line, err := watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for METADATA after SETMETADATA: %v", err)
	}
	for _, want := range []string{"* METADATA", "INBOX", "/private/comment"} {
		if !strings.Contains(line, want) {
			t.Errorf("METADATA response %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "hi") {
		t.Errorf("unsolicited METADATA must not carry values: %q", line)
	}

	if resp := other.cmd(`SETMETADATA "" (/private/vendor/test "v")`); !strings.Contains(resp, "OK") {
		t.Fatalf("SETMETADATA failed: %q", resp)
	}
	line, err = watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for METADATA after server SETMETADATA: %v", err)
	}
	for _, want := range []string{"* METADATA", "/private/vendor/test"} {
		if !strings.Contains(line, want) {
			t.Errorf("METADATA response %q is missing %q", line, want)
		}
	}

	// A deleted entry must still be reported (RFC 5465 section 5.6).
	if resp := other.cmd(`SETMETADATA INBOX (/private/comment NIL)`); !strings.Contains(resp, "OK") {
		t.Fatalf("SETMETADATA failed: %q", resp)
	}
	line, err = watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for METADATA after deletion: %v", err)
	}
	for _, want := range []string{"* METADATA", "/private/comment"} {
		if !strings.Contains(line, want) {
			t.Errorf("METADATA response %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "NIL") {
		t.Errorf("unsolicited METADATA must not carry values, not even NIL: %q", line)
	}
}

// mustListen opens a loopback listener for a test server.
func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return ln
}

// TestNotifyExpungeCommandResponses verifies that the NOTIFY filter only
// suppresses notifications, not command response data: RFC 9051 section 6.4.3
// and RFC 4315 section 2.1 require an untagged EXPUNGE for each removed message
// before the tagged OK of EXPUNGE / UID EXPUNGE, whatever the watch says.
func TestNotifyExpungeCommandResponses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		notify  string
		expunge string
	}{
		{name: "NotifyNone", notify: "NOTIFY NONE", expunge: "EXPUNGE"},
		{name: "NoSelectedGroup", notify: "NOTIFY SET (PERSONAL (MessageNew MessageExpunge))", expunge: "EXPUNGE"},
		{name: "UidExpunge", notify: "NOTIFY NONE", expunge: "UID EXPUNGE 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, closer := newNotifyTestServer(t)
			defer closer()

			c := dialNotifyTest(t, addr)
			defer c.close()
			c.login(t)
			c.cmd("CREATE INBOX")
			c.appendMessage(t, "INBOX")
			c.appendMessage(t, "INBOX")
			if resp := c.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
				t.Fatalf("SELECT failed: %q", resp)
			}
			if resp := c.cmd("%s", tc.notify); !strings.Contains(resp, "OK") {
				t.Fatalf("%s failed: %q", tc.notify, resp)
			}
			if resp := c.cmd(`STORE 1 +FLAGS (\Deleted)`); !strings.Contains(resp, "OK") {
				t.Fatalf("STORE failed: %q", resp)
			}

			resp := c.cmd("%s", tc.expunge)
			if !strings.Contains(resp, "* 1 EXPUNGE") {
				t.Fatalf("%s must report the removed message: %q", tc.expunge, resp)
			}

			// The client's sequence-number view must now match the server's.
			if resp := c.cmd("FETCH 1 (UID)"); !strings.Contains(resp, "UID 2") {
				t.Errorf("sequence numbers diverged after expunge: %q", resp)
			}
		})
	}
}

// TestNotifyOversizedWatchKeepsStreamInSync verifies that refusing an
// oversized NOTIFY SET does not desynchronise the command stream. The command
// is valid per RFC 5465 section 8 (neither event-groups nor many-mailboxes is
// length-bounded), so the server must consume all of it — literals included —
// before answering, and the next command must be parsed as a command.
func TestNotifyOversizedWatchKeepsStreamInSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{
			name: "TooManyGroups",
			cmd: strings.Repeat("(MAILBOXES box (MessageNew MessageExpunge)) ", 64) +
				"(MAILBOXES {5+}\r\nSNEAK (MessageNew MessageExpunge))",
		},
		{
			name: "TooManyMailboxesPerGroup",
			cmd: "(MAILBOXES (" + strings.Repeat("box ", 256) +
				"{5+}\r\nSNEAK) (MessageNew MessageExpunge))",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, closer := newNotifyTestServer(t)
			defer closer()

			c := dialNotifyTest(t, addr)
			defer c.close()
			c.login(t)

			resp := c.cmd("NOTIFY SET %s", tc.cmd)
			lines := strings.Split(strings.TrimRight(resp, "\r\n"), "\r\n")
			if len(lines) != 1 {
				t.Errorf("expected a single tagged response, got %q", resp)
			}
			if !strings.Contains(lines[len(lines)-1], "NO [NOTIFICATIONOVERFLOW]") {
				t.Errorf("expected NO [NOTIFICATIONOVERFLOW], got %q", resp)
			}

			// The connection must still be usable, and the literal's octets
			// must not have been parsed as a command.
			if resp := c.cmd("NOOP"); !strings.Contains(resp, "OK NOOP completed") {
				t.Errorf("connection desynchronised after the refusal: %q", resp)
			}
		})
	}
}

// TestNotifyMessageNewFetchAttNames verifies that the FETCH response of a
// MessageNew notification carries the data items the client asked for, spelled
// the way it asked for them (RFC 5465 section 5.2).
func TestNotifyMessageNewFetchAttNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fetchAtts string
		want      string
	}{
		{name: "BodyStructure", fetchAtts: "UID BODYSTRUCTURE", want: "BODYSTRUCTURE"},
		{name: "Body", fetchAtts: "UID BODY", want: "BODY "},
		{name: "ObsoleteRFC822Text", fetchAtts: "UID RFC822.TEXT", want: "RFC822.TEXT"},
		{name: "ObsoleteRFC822Header", fetchAtts: "UID RFC822.HEADER", want: "RFC822.HEADER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, closer := newNotifyTestServer(t)
			defer closer()

			watcher := dialNotifyTest(t, addr)
			defer watcher.close()
			watcher.login(t)
			watcher.cmd("CREATE INBOX")
			watcher.cmd("SELECT INBOX")
			if resp := watcher.cmd("NOTIFY SET (SELECTED (MessageNew (%s) MessageExpunge))", tc.fetchAtts); !strings.Contains(resp, "OK") {
				t.Fatalf("NOTIFY SET failed: %q", resp)
			}

			other := dialNotifyTest(t, addr)
			defer other.close()
			other.login(t)
			other.appendMessage(t, "INBOX")

			var fetch string
			for i := 0; i < 4 && fetch == ""; i++ {
				line, err := watcher.readLine(5 * time.Second)
				if err != nil {
					t.Fatalf("waiting for the FETCH notification: %v (last %q)", err, line)
				}
				if strings.Contains(line, "FETCH") {
					fetch = line
				}
			}
			if fetch == "" {
				t.Fatal("no FETCH notification arrived")
			}
			if !strings.Contains(fetch, tc.want) {
				t.Errorf("FETCH notification %q is missing %q", fetch, tc.want)
			}
		})
	}
}

// TestNotifyPumpFencedAcrossSelect verifies that no update queued for the
// previously selected mailbox is delivered once another mailbox is selected
// (RFC 5465 section 6.1: the SELECTED specifier follows the currently selected
// mailbox). The pump must be fenced across the switch.
func TestNotifyPumpFencedAcrossSelect(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("CREATE Other")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY SET (SELECTED (MessageNew MessageExpunge))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	for i := 0; i < 50; i++ {
		// Queue an event for INBOX and switch mailboxes concurrently with its
		// delivery.
		other.appendMessage(t, "INBOX")
		resp := watcher.cmd("SELECT Other")
		if !strings.Contains(resp, "OK") {
			t.Fatalf("SELECT Other failed: %q", resp)
		}

		// An update delivered before the switch took effect is legitimate — the
		// client sees it ahead of the SELECT response, while INBOX is still its
		// selected mailbox. What must never happen is delivery once the switch
		// has completed: Other is empty, so any EXISTS arriving now describes
		// INBOX and would corrupt the client's view of Other.
		if line, err := watcher.readLine(20 * time.Millisecond); err == nil {
			t.Fatalf("iteration %d: stray %q after Other became the selected mailbox", i, line)
		}
		if resp := watcher.cmd("NOOP"); strings.Contains(resp, "EXISTS") {
			t.Fatalf("iteration %d: stray %q at the next sync point", i, resp)
		}
		if resp := watcher.cmd("SELECT INBOX"); !strings.Contains(resp, "OK") {
			t.Fatalf("SELECT INBOX failed: %q", resp)
		}
	}
}

// TestNotifyOverflowResumesDelivery verifies the RFC 5465 section 5.8 flow: a
// frozen view that grows past the backend's limit produces an untagged
// OK [NOTIFICATIONOVERFLOW], after which the accumulated updates are delivered
// again instead of being held forever.
func TestNotifyOverflowResumesDelivery(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("SELECT INBOX")
	// No SELECTED specifier: message events for INBOX are suppressed and their
	// updates accumulate.
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxName))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	var overflowed bool
	for i := 0; i < 1200 && !overflowed; i++ {
		other.appendMessage(t, "INBOX")
		for {
			line, err := watcher.readLine(5 * time.Millisecond)
			if err != nil {
				break
			}
			if strings.Contains(line, "NOTIFICATIONOVERFLOW") {
				overflowed = true
				break
			}
		}
	}
	if !overflowed {
		t.Fatal("expected an OK [NOTIFICATIONOVERFLOW] once the frozen view grew past the limit")
	}

	// Notifications are off, but the client must not be left with a stale view:
	// the accumulated updates are delivered at the next sync point.
	resp := watcher.cmd("NOOP")
	if !strings.Contains(resp, "EXISTS") {
		t.Errorf("expected the accumulated updates to be delivered after the overflow, got %q", resp)
	}
}

// TestNotifyMailboxNameAffectedNames verifies RFC 5465 section 5.4: creating or
// deleting a mailbox affects the mailbox itself and its direct parent, and the
// LIST responses report child status.
func TestNotifyMailboxNameAffectedNames(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxName))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	if resp := other.cmd("CREATE Parent/Child"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}

	var sawChild, sawParent bool
	for i := 0; i < 2; i++ {
		line, err := watcher.readLine(5 * time.Second)
		if err != nil {
			t.Fatalf("waiting for LIST responses: %v", err)
		}
		switch {
		case strings.Contains(line, "Parent/Child"):
			sawChild = true
			if !strings.Contains(line, "\\HasNoChildren") {
				t.Errorf("expected \\HasNoChildren for the new mailbox: %q", line)
			}
		case strings.Contains(line, "Parent"):
			sawParent = true
			// Parent itself was never created, but it now has a child.
			if !strings.Contains(line, "\\NonExistent") || !strings.Contains(line, "\\HasChildren") {
				t.Errorf("expected the affected parent as \\NonExistent \\HasChildren: %q", line)
			}
		}
	}
	if !sawChild || !sawParent {
		t.Errorf("expected LIST responses for the mailbox and its parent (child=%v parent=%v)", sawChild, sawParent)
	}
}

// TestNotifyAclChangeReportsMailboxName verifies RFC 5465 section 5.4 (losing
// the 'l' right counts as deletion) and section 5.9 (losing another required
// right is reported with \NoAccess).
func TestNotifyAclChangeReportsMailboxName(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	if resp := watcher.cmd("CREATE Shared"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxName))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	// Losing 'r' while keeping 'l': \NoAccess.
	if resp := other.cmd("SETACL Shared user -r"); !strings.Contains(resp, "OK") {
		t.Fatalf("SETACL failed: %q", resp)
	}
	line, err := watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for the \\NoAccess LIST: %v", err)
	}
	if !strings.Contains(line, "\\NoAccess") || !strings.Contains(line, "Shared") {
		t.Errorf("expected a \\NoAccess LIST for Shared, got %q", line)
	}

	// Losing 'l': reported as a deletion.
	if resp := other.cmd("SETACL Shared user -l"); !strings.Contains(resp, "OK") {
		t.Fatalf("SETACL failed: %q", resp)
	}
	line, err = watcher.readLine(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for the \\NonExistent LIST: %v", err)
	}
	if !strings.Contains(line, "\\NonExistent") || !strings.Contains(line, "Shared") {
		t.Errorf("expected a \\NonExistent LIST for Shared, got %q", line)
	}
}

// TestNotifySelfCreatedMessageNoFetch verifies RFC 5465 section 5.2: "A FETCH
// response SHOULD NOT be generated for a new message created by the client on
// this particular connection".
func TestNotifySelfCreatedMessageNoFetch(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	c := dialNotifyTest(t, addr)
	defer c.close()
	c.login(t)
	c.cmd("CREATE INBOX")
	c.cmd("SELECT INBOX")
	if resp := c.cmd("NOTIFY SET (SELECTED (MessageNew (UID) MessageExpunge))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	// This connection's own APPEND: EXISTS is still required, a FETCH is not.
	c.appendMessage(t, "INBOX")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		line, err := c.readLine(100 * time.Millisecond)
		if err != nil {
			continue
		}
		if strings.Contains(line, "FETCH") {
			t.Fatalf("unexpected FETCH for a message created by this connection: %q", line)
		}
	}

	// A message from another connection still gets one.
	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	other.appendMessage(t, "INBOX")

	var sawFetch bool
	for i := 0; i < 4 && !sawFetch; i++ {
		line, err := c.readLine(5 * time.Second)
		if err != nil {
			break
		}
		sawFetch = strings.Contains(line, "FETCH")
	}
	if !sawFetch {
		t.Error("expected a FETCH notification for a message from another connection")
	}
}

// TestNotifyEventsSurviveMailboxSwitch verifies that fencing the pump across a
// mailbox switch only pauses delivery: events raised while the pump is stopped
// must still reach the client, not be dropped.
func TestNotifyEventsSurviveMailboxSwitch(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY SET (PERSONAL (MailboxName))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	const count = 30
	seen := make(map[string]bool)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("box%02d", i)
		if resp := other.cmd("CREATE %s", name); !strings.Contains(resp, "OK") {
			t.Fatalf("CREATE %s failed: %q", name, resp)
		}
		// Re-select in the middle of the notification traffic: this stops and
		// restarts the pump.
		resp := watcher.cmd("SELECT INBOX")
		for _, line := range strings.Split(resp, "\r\n") {
			if strings.HasPrefix(line, "* LIST") {
				seen[strings.Trim(line[strings.LastIndex(line, " ")+1:], `"`)] = true
			}
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < count && time.Now().Before(deadline) {
		line, err := watcher.readLine(200 * time.Millisecond)
		if err != nil {
			continue
		}
		if strings.HasPrefix(line, "* LIST") {
			seen[strings.Trim(strings.TrimRight(line, "\r\n")[strings.LastIndex(strings.TrimRight(line, "\r\n"), " ")+1:], `"`)] = true
		}
	}

	var missing []string
	for i := 0; i < count; i++ {
		if name := fmt.Sprintf("box%02d", i); !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d MailboxName notifications were lost across the mailbox switches: %v", len(missing), count, missing)
	}
}

// TestNotifySelectedDelayedReleasedInIdle verifies RFC 5465 section 6.1.2: a
// SELECTED-DELAYED watch withholds MessageExpunge only "till a NOOP or an IDLE
// command has been issued". Holding it for the whole IDLE would also block
// every later update, since updates are delivered in order.
func TestNotifySelectedDelayedReleasedInIdle(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.appendMessage(t, "INBOX")
	watcher.appendMessage(t, "INBOX")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	other.cmd("SELECT INBOX")
	other.cmd(`STORE 1 +FLAGS (\Deleted)`)
	if resp := other.cmd("EXPUNGE"); !strings.Contains(resp, "OK") {
		t.Fatalf("EXPUNGE failed: %q", resp)
	}

	// Enter IDLE: the delayed expunge, and the append queued behind it, must
	// be delivered.
	if _, err := watcher.conn.Write([]byte("T99 IDLE\r\n")); err != nil {
		t.Fatalf("write IDLE: %v", err)
	}
	if line, err := watcher.readLine(5 * time.Second); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("expected a continuation request for IDLE, got %q (%v)", line, err)
	}
	other.appendMessage(t, "INBOX")

	var sawExpunge, sawExists bool
	deadline := time.Now().Add(5 * time.Second)
	for (!sawExpunge || !sawExists) && time.Now().Before(deadline) {
		line, err := watcher.readLine(500 * time.Millisecond)
		if err != nil {
			continue
		}
		if strings.Contains(line, "EXPUNGE") {
			sawExpunge = true
		}
		if strings.Contains(line, "EXISTS") {
			sawExists = true
		}
	}
	watcher.conn.Write([]byte("DONE\r\n"))
	if !sawExpunge {
		t.Error("the delayed EXPUNGE was not released during IDLE")
	}
	if !sawExists {
		t.Error("the EXISTS queued behind the delayed EXPUNGE never arrived")
	}
}

// TestCommandErrorLiteralRecovery covers the ways a failed command can leave
// literal octets in the stream. Either they are skipped and the connection
// keeps working, or the connection is closed — what must never happen is the
// next command being parsed from inside client data (RFC 9051 section 2.2.1).
func TestCommandErrorLiteralRecovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmd     string
		survive bool // connection is expected to remain usable
	}{
		{
			name:    "SmallNonSyncLiteralInFailedCommand",
			cmd:     "NOTIFY SET (BOGUSSPEC {5+}\r\nSNEAK (MessageNew MessageExpunge))",
			survive: false, // announcement never parsed: cannot resynchronize
		},
		{
			name:    "RejectedNonSyncLiteral",
			cmd:     "LOGIN {5000+}\r\n" + strings.Repeat("A", 5000) + " pass",
			survive: true, // size refused, octets skipped
		},
		{
			name:    "RejectedSyncLiteral",
			cmd:     "LOGIN {5000}",
			survive: true, // never sent: nothing to skip
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, closer := newNotifyTestServer(t)
			defer closer()

			c := dialNotifyTest(t, addr)
			defer c.close()

			// The server must answer, rather than block waiting for octets the
			// client is not going to send.
			c.tag++
			line := fmt.Sprintf("T%d %s\r\n", c.tag, tc.cmd)
			if _, err := c.conn.Write([]byte(line)); err != nil {
				t.Fatalf("write: %v", err)
			}
			resp, err := c.readLine(5 * time.Second)
			if err != nil {
				t.Fatalf("no response to the failed command: %v", err)
			}
			t.Logf("response: %q", resp)

			// Whatever follows must not be executed as a command.
			if _, err := c.conn.Write([]byte("T90 NOOP\r\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
			next, err := c.readLine(5 * time.Second)
			switch {
			case tc.survive:
				if err != nil {
					t.Fatalf("connection unusable after recovery: %v", err)
				}
				if !strings.Contains(next, "T90 OK") && !strings.Contains(next, "T90 ") {
					t.Errorf("expected a response to the next command, got %q", next)
				}
			default:
				if err == nil && !strings.Contains(next, "BYE") {
					t.Errorf("expected the connection to be closed, got %q", next)
				}
			}
		})
	}
}

// TestNotifyNoneSilencesIdle verifies that IDLE reports nothing once the client
// has asked for no events at all with NOTIFY NONE (RFC 5465 section 3.1).
func TestNotifyNoneSilencesIdle(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY NONE"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY NONE failed: %q", resp)
	}

	if _, err := watcher.conn.Write([]byte("T90 IDLE\r\n")); err != nil {
		t.Fatalf("write IDLE: %v", err)
	}
	if line, err := watcher.readLine(5 * time.Second); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("expected a continuation request for IDLE, got %q (%v)", line, err)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	other.appendMessage(t, "INBOX")

	if line, err := watcher.readLine(500 * time.Millisecond); err == nil {
		t.Errorf("IDLE pushed %q after NOTIFY NONE", line)
	}
	watcher.conn.Write([]byte("DONE\r\n"))
}

// TestNotifyNoneBoundsSuppressedQueue verifies that the updates a NOTIFY NONE
// client's selected mailbox accumulates — undeliverable while the suppression
// lasts, and never looked at by a per-command Poll — are still bounded: the
// backend declares a notification overflow (RFC 5465 section 5.8) from the
// pump the library keeps running for NOTIFY NONE, and delivery resumes.
func TestNotifyNoneBoundsSuppressedQueue(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY NONE"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY NONE failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)

	var overflowed bool
	for i := 0; i < 1200 && !overflowed; i++ {
		other.appendMessage(t, "INBOX")
		for {
			line, err := watcher.readLine(5 * time.Millisecond)
			if err != nil {
				break
			}
			if strings.Contains(line, "NOTIFICATIONOVERFLOW") {
				overflowed = true
				break
			}
			t.Errorf("NOTIFY NONE client received %q before the overflow", line)
		}
	}
	if !overflowed {
		t.Fatal("expected an OK [NOTIFICATIONOVERFLOW] once the frozen view grew past the limit")
	}

	resp := watcher.cmd("NOOP")
	if !strings.Contains(resp, "EXISTS") {
		t.Errorf("expected the accumulated updates to be delivered after the overflow, got %q", resp)
	}
}

// TestNotifyOverflowDuringIdle verifies that a notification overflow declared
// while the client is idling puts IDLE back in charge of delivery: with the
// watch gone the connection is under the pre-NOTIFY rules, and IDLE pushes the
// updates that had accumulated, then the new ones, without waiting for DONE.
func TestNotifyOverflowDuringIdle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify string
	}{
		{name: "NotifyNone", notify: "NOTIFY NONE"},
		{name: "SelectedOmitted", notify: "NOTIFY SET (PERSONAL (MailboxName))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _, closer := newNotifyTestServer(t)
			defer closer()

			watcher := dialNotifyTest(t, addr)
			defer watcher.close()
			watcher.login(t)
			watcher.cmd("CREATE INBOX")
			watcher.cmd("SELECT INBOX")
			if resp := watcher.cmd("%s", tc.notify); !strings.Contains(resp, "OK") {
				t.Fatalf("%s failed: %q", tc.notify, resp)
			}

			if _, err := watcher.conn.Write([]byte("T90 IDLE\r\n")); err != nil {
				t.Fatalf("write IDLE: %v", err)
			}
			if line, err := watcher.readLine(5 * time.Second); err != nil || !strings.HasPrefix(line, "+") {
				t.Fatalf("expected a continuation request for IDLE, got %q (%v)", line, err)
			}

			other := dialNotifyTest(t, addr)
			defer other.close()
			other.login(t)
			for i := 0; i < 1100; i++ {
				other.appendMessage(t, "INBOX")
			}

			// The overflow comes first, then IDLE flushes the frozen updates.
			var sawOverflow, sawExists bool
			for !sawExists {
				line, err := watcher.readLine(5 * time.Second)
				if err != nil {
					t.Fatalf("waiting for IDLE to resume delivery (overflow seen: %v): %v", sawOverflow, err)
				}
				switch {
				case strings.Contains(line, "NOTIFICATIONOVERFLOW"):
					sawOverflow = true
				case strings.Contains(line, "EXISTS"):
					if !sawOverflow {
						t.Fatalf("EXISTS pushed before the overflow: %q", line)
					}
					sawExists = true
				}
			}

			// And it keeps delivering: a new message is pushed at once.
			other.appendMessage(t, "INBOX")
			for {
				line, err := watcher.readLine(5 * time.Second)
				if err != nil {
					t.Fatalf("expected IDLE to push the next EXISTS: %v", err)
				}
				if strings.Contains(line, "1101 EXISTS") {
					break
				}
			}

			watcher.conn.Write([]byte("DONE\r\n"))
			for {
				line, err := watcher.readLine(5 * time.Second)
				if err != nil {
					t.Fatalf("waiting for IDLE completion: %v", err)
				}
				if strings.HasPrefix(line, "T90 ") {
					if !strings.Contains(line, "OK") {
						t.Fatalf("IDLE failed: %q", line)
					}
					break
				}
			}
		})
	}
}

// TestNotifyRejectedFirstWatchLeavesDefaultDelivery verifies that a NOTIFY SET
// refused with BADEVENT changes nothing (RFC 5465 section 3.1: the effect of a
// NOTIFY command lasts until the next successful one): if it was the first
// NOTIFY, the connection is still under the pre-NOTIFY rules and IDLE keeps
// pushing the selected mailbox's updates.
func TestNotifyRejectedFirstWatchLeavesDefaultDelivery(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY SET (SELECTED (MessageNew MessageExpunge AnnotationChange))"); !strings.Contains(resp, "NO [BADEVENT") {
		t.Fatalf("expected the watch to be refused with BADEVENT, got %q", resp)
	}

	if _, err := watcher.conn.Write([]byte("T90 IDLE\r\n")); err != nil {
		t.Fatalf("write IDLE: %v", err)
	}
	if line, err := watcher.readLine(5 * time.Second); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("expected a continuation request for IDLE, got %q (%v)", line, err)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	other.appendMessage(t, "INBOX")

	line, err := watcher.readLine(5 * time.Second)
	if err != nil || !strings.Contains(line, "1 EXISTS") {
		t.Fatalf("expected IDLE to push EXISTS after a refused NOTIFY, got %q (%v)", line, err)
	}
	watcher.conn.Write([]byte("DONE\r\n"))
}
