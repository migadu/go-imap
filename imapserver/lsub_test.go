package imapserver_test

import (
	"bufio"
	"bytes"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// lsubSession wraps a memserver session but overrides List so we can drive the
// exact LIST data the server layer must translate into an LSUB response. It
// records the options the server passed, so the test can assert that LSUB was
// dispatched as LIST (SUBSCRIBED RECURSIVEMATCH).
type lsubSession struct {
	imapserver.Session
	gotSubscribed     bool
	gotRecursiveMatch bool
}

func (s *lsubSession) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	s.gotSubscribed = options.SelectSubscribed
	s.gotRecursiveMatch = options.SelectRecursiveMatch

	// Model the RFC 3501 §6.3.9 situation: "Foo/Baz" is subscribed, "Foo" is
	// not. A RECURSIVEMATCH-aware session reports the non-subscribed parent
	// "Foo" only because it has a subscribed child (CHILDINFO), and reports the
	// subscribed child directly with \Subscribed.
	if err := w.WriteList(&imap.ListData{
		Mailbox:   "Foo",
		Delim:     '/',
		ChildInfo: &imap.ListDataChildInfo{Subscribed: true},
	}); err != nil {
		return err
	}
	return w.WriteList(&imap.ListData{
		Mailbox: "Foo/Baz",
		Delim:   '/',
		Attrs:   []imap.MailboxAttr{imap.MailboxAttrSubscribed},
	})
}

// TestLSubRecursiveMatchToNoselect verifies that the server dispatches LSUB as
// LIST (SUBSCRIBED RECURSIVEMATCH) and renders the result in RFC 3501 LSUB form:
//   - a non-subscribed parent carrying CHILDINFO becomes \Noselect (§6.3.9);
//   - a directly-subscribed mailbox is reported without the LIST-EXTENDED-only
//     \Subscribed attribute.
func TestLSubRecursiveMatchToNoselect(t *testing.T) {
	const username, password = "user", "pass"

	memUser := imapmemserver.NewUser(username, password)
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	var sess *lsubSession
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			sess = &lsubSession{Session: memServer.NewSession()}
			return sess, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
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
	readUntilTag := func(tag string) string {
		var buf bytes.Buffer
		for {
			line, err := br.ReadString('\n')
			buf.WriteString(line)
			if strings.HasPrefix(line, tag+" ") {
				return buf.String()
			}
			if err != nil {
				t.Fatalf("reading response for tag %q: %v (got %q)", tag, err, buf.String())
			}
		}
	}

	if _, err := br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("read greeting: %v", err)
	}
	send("a LOGIN " + username + " " + password)
	if resp := readUntilTag("a"); !strings.Contains(resp, "a OK") {
		t.Fatalf("LOGIN failed: %q", resp)
	}

	send(`b LSUB "" "%"`)
	resp := readUntilTag("b")
	if !strings.Contains(resp, "b OK") {
		t.Fatalf("LSUB failed: %q", resp)
	}

	// The server must have translated LSUB into LIST (SUBSCRIBED RECURSIVEMATCH).
	if !sess.gotSubscribed || !sess.gotRecursiveMatch {
		t.Errorf("LSUB should dispatch as LIST (SUBSCRIBED RECURSIVEMATCH); got SelectSubscribed=%v SelectRecursiveMatch=%v",
			sess.gotSubscribed, sess.gotRecursiveMatch)
	}

	fooAttrs, ok := lsubLineAttrs(resp, "Foo")
	if !ok {
		t.Fatalf("no LSUB line for parent %q in response:\n%s", "Foo", resp)
	}
	if !strings.Contains(fooAttrs, `\Noselect`) {
		t.Errorf("non-subscribed parent Foo must be flagged \\Noselect (RFC 3501 §6.3.9); attrs=(%s)", fooAttrs)
	}

	bazAttrs, ok := lsubLineAttrs(resp, "Foo/Baz")
	if !ok {
		t.Fatalf("no LSUB line for subscribed child %q in response:\n%s", "Foo/Baz", resp)
	}
	if strings.Contains(bazAttrs, `\Subscribed`) {
		t.Errorf("LSUB must not emit the LIST-EXTENDED-only \\Subscribed attribute; attrs=(%s)", bazAttrs)
	}
	if strings.Contains(bazAttrs, `\Noselect`) {
		t.Errorf("directly-subscribed mailbox Foo/Baz must not be \\Noselect; attrs=(%s)", bazAttrs)
	}

	send("z LOGOUT")
	readUntilTag("z")
}

// lsubLineAttrs returns the attribute list of the "* LSUB (attrs) \"/\" <mailbox>"
// line whose mailbox equals name, matching the mailbox token exactly (so "Foo"
// does not accidentally match "Foo/Baz").
func lsubLineAttrs(resp, name string) (string, bool) {
	re := regexp.MustCompile(`(?m)^\* LSUB \(([^)]*)\) "[^"]*" (.+?)\r?$`)
	for _, m := range re.FindAllStringSubmatch(resp, -1) {
		mailbox := strings.TrimSpace(m[2])
		mailbox = strings.Trim(mailbox, `"`)
		if mailbox == name {
			return m[1], true
		}
	}
	return "", false
}
