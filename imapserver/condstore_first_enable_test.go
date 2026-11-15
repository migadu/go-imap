package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// firstEnableHighestModSeq is what highestModSeqSession reports. It is unlike
// any mod-sequence the in-memory backend assigns to a handful of messages, so a
// response carrying it can only have come from SessionHighestModSeq, not from
// SELECT.
const firstEnableHighestModSeq = 4242

// highestModSeqSession reports a fixed HIGHESTMODSEQ for the selected mailbox
// and can make SELECT answer NOMODSEQ.
type highestModSeqSession struct {
	imapserver.Session
	noModSeq bool
}

func (s *highestModSeqSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := s.Session.Select(ctx, mailbox, options)
	if err == nil && s.noModSeq {
		data.HighestModSeq = 0
	}
	return data, err
}

func (s *highestModSeqSession) HighestModSeq() uint64 {
	return firstEnableHighestModSeq
}

// newFirstEnableTestConn logs in to a server whose INBOX holds two messages,
// without selecting it.
func newFirstEnableTestConn(t *testing.T, noModSeq bool) *storeTestConn {
	t.Helper()

	memUser := imapmemserver.NewUser("user", "pass")
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &highestModSeqSession{Session: memServer.NewSession(), noModSeq: noModSeq}, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:  {},
			imap.CapCondStore:  {},
			imap.CapEnable:     {},
			imap.CapListStatus: {},
		},
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn), addr: ln.Addr().String()}
	if _, err := c.br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("read greeting: %v", err)
	}
	if resp := c.do("login", "LOGIN user pass"); !strings.Contains(resp, "login OK") {
		t.Fatalf("LOGIN failed: %q", resp)
	}
	c.appendMessage("append1")
	c.appendMessage("append2")
	return c
}

// highestModSeqLines returns the untagged OK [HIGHESTMODSEQ ...] lines of resp.
func highestModSeqLines(resp string) []string {
	var lines []string
	for _, line := range strings.Split(resp, "\r\n") {
		if strings.HasPrefix(line, "* OK [HIGHESTMODSEQ ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestFirstCondStoreEnablingCommandReturnsHighestModSeq checks RFC 7162 §3.1:
// "A first CONDSTORE enabling command executed in the session with a mailbox
// selected MUST cause the server to return HIGHESTMODSEQ (Section 3.1.2.1) for
// the mailbox (if any is selected), unless the server has sent a NOMODSEQ
// (Section 3.1.2.2) response code when the currently selected mailbox was
// selected."
func TestFirstCondStoreEnablingCommandReturnsHighestModSeq(t *testing.T) {
	want := "* OK [HIGHESTMODSEQ 4242] "

	for _, tt := range []struct {
		name string
		cmd  string
		ok   string // tagged completion prefix
	}{
		{"FETCH MODSEQ", "FETCH 1:2 (FLAGS MODSEQ)", "OK"},
		{"UID FETCH CHANGEDSINCE", "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1)", "OK"},
		{"SEARCH MODSEQ", "SEARCH MODSEQ 1", "OK"},
		{"STORE UNCHANGEDSINCE", `STORE 1 (UNCHANGEDSINCE 1000000) +FLAGS (\Flagged)`, "OK"},
		// The always-fail probe completes with OK [MODIFIED], the OK-typed
		// error path of the command loop.
		{"STORE UNCHANGEDSINCE 0", `STORE 1 (UNCHANGEDSINCE 0) +FLAGS (\Flagged)`, "OK [MODIFIED 1]"},
		{"STATUS HIGHESTMODSEQ", "STATUS INBOX (HIGHESTMODSEQ)", "OK"},
		{"LIST RETURN STATUS HIGHESTMODSEQ", `LIST "" INBOX RETURN (STATUS (HIGHESTMODSEQ))`, "OK"},
		{"ENABLE CONDSTORE", "ENABLE CONDSTORE", "OK"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newFirstEnableTestConn(t, false)
			if resp := c.do("select", "SELECT INBOX"); !strings.Contains(resp, "select OK") {
				t.Fatalf("SELECT failed: %q", resp)
			}

			resp := c.do("first", tt.cmd)
			if !strings.Contains(resp, "first "+tt.ok) {
				t.Fatalf("%v = %q, want a tagged %v", tt.cmd, resp, tt.ok)
			}
			lines := highestModSeqLines(resp)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], want) {
				t.Fatalf("%v = %q, want one %q response", tt.cmd, resp, want)
			}

			// Only the first enabling command returns it.
			resp = c.do("second", "FETCH 1 (MODSEQ)")
			if lines := highestModSeqLines(resp); len(lines) != 0 {
				t.Errorf("second enabling command FETCH 1 (MODSEQ) = %q, want no HIGHESTMODSEQ", resp)
			}
		})
	}
}

// TestFirstCondStoreEnablingCommandNoHighestModSeq covers the cases where RFC
// 7162 §3.1 asks for nothing beyond the command's own responses.
func TestFirstCondStoreEnablingCommandNoHighestModSeq(t *testing.T) {
	t.Run("not an enabling command", func(t *testing.T) {
		c := newFirstEnableTestConn(t, false)
		c.do("select", "SELECT INBOX")
		resp := c.do("fetch", "FETCH 1:2 (FLAGS)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH 1:2 (FLAGS) = %q, want no HIGHESTMODSEQ", resp)
		}
	})

	t.Run("enabled before SELECT", func(t *testing.T) {
		c := newFirstEnableTestConn(t, false)
		resp := c.do("enable", "ENABLE CONDSTORE")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("ENABLE CONDSTORE with no mailbox selected = %q, want no HIGHESTMODSEQ", resp)
		}
		c.do("select", "SELECT INBOX")
		resp = c.do("fetch", "FETCH 1 (MODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH 1 (MODSEQ) after ENABLE CONDSTORE = %q, want no HIGHESTMODSEQ", resp)
		}
	})

	t.Run("STATUS HIGHESTMODSEQ before SELECT", func(t *testing.T) {
		c := newFirstEnableTestConn(t, false)
		resp := c.do("status", "STATUS INBOX (HIGHESTMODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("STATUS INBOX (HIGHESTMODSEQ) with no mailbox selected = %q, want no HIGHESTMODSEQ", resp)
		}
		c.do("select", "SELECT INBOX")
		resp = c.do("fetch", "FETCH 1 (MODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH 1 (MODSEQ) after STATUS (HIGHESTMODSEQ) = %q, want no HIGHESTMODSEQ", resp)
		}
	})

	t.Run("selected with NOMODSEQ", func(t *testing.T) {
		c := newFirstEnableTestConn(t, true)
		if resp := c.do("select", "SELECT INBOX"); !strings.Contains(resp, "[NOMODSEQ]") {
			t.Fatalf("SELECT = %q, want NOMODSEQ", resp)
		}
		resp := c.do("fetch", "FETCH 1 (MODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH 1 (MODSEQ) on a NOMODSEQ mailbox = %q, want no HIGHESTMODSEQ", resp)
		}
	})

	// SELECT (CONDSTORE) with a mailbox selected reports the new mailbox's
	// HIGHESTMODSEQ itself, once.
	t.Run("SELECT CONDSTORE", func(t *testing.T) {
		c := newFirstEnableTestConn(t, false)
		c.do("select", "SELECT INBOX")
		resp := c.do("reselect", "SELECT INBOX (CONDSTORE)")
		lines := highestModSeqLines(resp)
		if len(lines) != 1 || strings.HasPrefix(lines[0], "* OK [HIGHESTMODSEQ 4242]") {
			t.Errorf("SELECT INBOX (CONDSTORE) = %q, want only the SELECT's own HIGHESTMODSEQ", resp)
		}
	})
}

// TestBadCondStoreEnablingCommandIsNotTheFirst checks that an enabling command
// answered BAD, a protocol error (RFC 9051 §2.2.2), leaves the client as it
// was, although CONDSTORE was marked while the command parsed: no MODSEQ in
// later FETCH responses, and the next enabling command that runs is the first
// one and returns HIGHESTMODSEQ.
func TestBadCondStoreEnablingCommandIsNotTheFirst(t *testing.T) {
	for _, cmd := range []string{
		// An unknown modifier, refused after CHANGEDSINCE.
		"UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1 BOGUS)",
		"FETCH 1 (FLAGS MODSEQ) EXTRA",
		`STORE 1 (UNCHANGEDSINCE 1000000) +FLAGS (\Flagged) EXTRA`,
	} {
		t.Run(cmd, func(t *testing.T) {
			c := newFirstEnableTestConn(t, false)
			c.do("select", "SELECT INBOX")

			resp := c.do("bad", cmd)
			if !strings.HasPrefix(resp, "bad BAD ") {
				t.Fatalf("%v = %q, want only a tagged BAD", cmd, resp)
			}

			resp = c.do("store", `STORE 2 +FLAGS (\Seen)`)
			if !containsFlag(resp, imap.FlagSeen) {
				t.Fatalf("STORE = %q, want the untagged FETCH with the new flags", resp)
			}
			if strings.Contains(resp, "MODSEQ") {
				t.Errorf("STORE after %v = %q, want no MODSEQ", cmd, resp)
			}

			resp = c.do("fetch", "FETCH 1 (MODSEQ)")
			if lines := highestModSeqLines(resp); len(lines) != 1 {
				t.Errorf("FETCH 1 (MODSEQ) after %v = %q, want one HIGHESTMODSEQ", cmd, resp)
			}
		})
	}
}
