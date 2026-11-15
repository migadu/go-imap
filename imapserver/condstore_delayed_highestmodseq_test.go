package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// watermarkSession reports a settable HIGHESTMODSEQ: the mod-sequence up to
// which the backend has sent every change. It stands in for a backend that
// still holds changes above it, such as an EXPUNGE a FETCH, STORE or SEARCH
// may not send (RFC 9051 §7.5.1).
type watermarkSession struct {
	imapserver.Session
	noModSeq  bool
	watermark atomic.Uint64
}

func (s *watermarkSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := s.Session.Select(ctx, mailbox, options)
	if err == nil && s.noModSeq {
		data.HighestModSeq = 0
	}
	return data, err
}

func (s *watermarkSession) HighestModSeq() uint64 {
	return s.watermark.Load()
}

// newWatermarkTestConn logs in to a server whose INBOX holds two messages, with
// mod-sequences 2 and 3, without selecting it. Peers dialed from the returned
// connection get sessions of their own.
func newWatermarkTestConn(t *testing.T, noModSeq bool) (*storeTestConn, *watermarkSession) {
	t.Helper()

	memUser := imapmemserver.NewUser("user", "pass")
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	sessions := make(chan *watermarkSession, 4)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := &watermarkSession{Session: memServer.NewSession(), noModSeq: noModSeq}
			sessions <- s
			return s, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapCondStore: {},
			imap.CapEnable:    {},
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
	session := <-sessions
	if resp := c.do("login", "LOGIN user pass"); !strings.Contains(resp, "login OK") {
		t.Fatalf("LOGIN failed: %q", resp)
	}
	c.appendMessage("append1")
	c.appendMessage("append2")
	return c, session
}

// checkHighestModSeqBeforeTag checks that resp carries exactly one
// HIGHESTMODSEQ response, with value want, after every untagged FETCH and
// right before the tagged completion.
func checkHighestModSeqBeforeTag(t *testing.T, cmd, resp, want string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(resp, "\r\n"), "\r\n")
	if got := highestModSeqLines(resp); len(got) != 1 || !strings.HasPrefix(got[0], "* OK [HIGHESTMODSEQ "+want+"] ") {
		t.Fatalf("%v = %q, want one * OK [HIGHESTMODSEQ %v]", cmd, resp, want)
	}
	if len(lines) < 2 || !strings.HasPrefix(lines[len(lines)-2], "* OK [HIGHESTMODSEQ ") {
		t.Fatalf("%v = %q, want HIGHESTMODSEQ after the FETCH responses, right before the tagged completion", cmd, resp)
	}
}

// TestHighestModSeqBelowDelayedExpunge checks RFC 7162 §3.2: "if the server
// sends MODSEQ FETCH data items while EXPUNGE (or VANISHED) replies with lower
// mod-sequences being delayed, the server MUST send the HIGHESTMODSEQ response
// code with a lower value than the EXPUNGE's mod-sequence." Without it the
// client takes "the highest value among all MODSEQ FETCH data items received
// since the last tagged response" (RFC 7162 §6) and skips the delayed EXPUNGE
// when it resynchronizes after losing the connection.
func TestHighestModSeqBelowDelayedExpunge(t *testing.T) {
	c, session := newWatermarkTestConn(t, false)
	c.do("enable", "ENABLE CONDSTORE")
	if resp := c.do("select", "SELECT INBOX"); !strings.Contains(resp, "* OK [HIGHESTMODSEQ 3]") {
		t.Fatalf("SELECT = %q, want HIGHESTMODSEQ 3", resp)
	}
	session.watermark.Store(3)

	// Mod-sequences 4 and 5.
	peer := c.dialPeer()
	peer.do("pstore", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	peer.do("pexpunge", "EXPUNGE")

	// The STORE answers with MODSEQ (6) and may not send the EXPUNGE (5).
	resp := c.do("store", `STORE 2 +FLAGS (\Seen)`)
	if strings.Contains(resp, "EXPUNGE") {
		t.Fatalf("STORE = %q, want the EXPUNGE held back (RFC 9051 §7.5.1)", resp)
	}
	if !strings.Contains(resp, "MODSEQ (6)") {
		t.Fatalf("STORE = %q, want MODSEQ (6)", resp)
	}
	checkHighestModSeqBeforeTag(t, "STORE", resp, "3")

	resp = c.do("noop", "NOOP")
	if !strings.Contains(resp, "* 1 EXPUNGE") {
		t.Fatalf("NOOP = %q, want the delayed EXPUNGE", resp)
	}
	if lines := highestModSeqLines(resp); len(lines) != 0 {
		t.Errorf("NOOP = %q, want no HIGHESTMODSEQ: it sent no MODSEQ FETCH data item", resp)
	}
}

// TestHighestModSeqBelowFetchedModSeq covers the commands that write MODSEQ
// FETCH data items above the session's watermark, solicited or not.
func TestHighestModSeqBelowFetchedModSeq(t *testing.T) {
	for _, tt := range []struct {
		name string
		cmd  string
	}{
		{"FETCH MODSEQ", "FETCH 1:2 (FLAGS MODSEQ)"},
		{"UID FETCH CHANGEDSINCE", "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1)"},
		{"STORE", `STORE 1 +FLAGS (\Flagged)`},
		{"STORE UNCHANGEDSINCE SILENT", `STORE 1 (UNCHANGEDSINCE 1000) +FLAGS.SILENT (\Flagged)`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, session := newWatermarkTestConn(t, false)
			c.do("enable", "ENABLE CONDSTORE")
			c.do("select", "SELECT INBOX")
			session.watermark.Store(2)

			resp := c.do("cmd", tt.cmd)
			if !strings.Contains(resp, "cmd OK") || !strings.Contains(resp, "MODSEQ (") {
				t.Fatalf("%v = %q, want MODSEQ FETCH data and a tagged OK", tt.cmd, resp)
			}
			checkHighestModSeqBeforeTag(t, tt.cmd, resp, "2")

			// Each tagged response starts the count anew.
			resp = c.do("noop", "NOOP")
			if lines := highestModSeqLines(resp); len(lines) != 0 {
				t.Errorf("NOOP after %v = %q, want no HIGHESTMODSEQ", tt.cmd, resp)
			}
		})
	}

	// MODSEQ (4) in an unsolicited FETCH counts as well.
	t.Run("unsolicited FETCH", func(t *testing.T) {
		c, session := newWatermarkTestConn(t, false)
		c.do("enable", "ENABLE CONDSTORE")
		c.do("select", "SELECT INBOX")
		session.watermark.Store(3)

		peer := c.dialPeer()
		peer.do("pstore", `STORE 1 +FLAGS.SILENT (\Flagged)`)

		resp := c.do("noop", "NOOP")
		if !strings.Contains(resp, "MODSEQ (4)") {
			t.Fatalf("NOOP = %q, want the peer's flag change with MODSEQ (4)", resp)
		}
		checkHighestModSeqBeforeTag(t, "NOOP", resp, "3")
	})

	// The first CONDSTORE enabling command returns HIGHESTMODSEQ once (RFC
	// 7162 §3.1), not once more for its MODSEQ items.
	t.Run("first CONDSTORE enabling command", func(t *testing.T) {
		c, session := newWatermarkTestConn(t, false)
		c.do("select", "SELECT INBOX")
		session.watermark.Store(2)

		resp := c.do("fetch", "FETCH 1:2 (FLAGS MODSEQ)")
		checkHighestModSeqBeforeTag(t, "FETCH 1:2 (FLAGS MODSEQ)", resp, "2")
	})
}

// TestHighestModSeqNotBelowFetchedModSeq covers the cases where nothing is
// needed beyond the command's own responses.
func TestHighestModSeqNotBelowFetchedModSeq(t *testing.T) {
	t.Run("MODSEQ items covered", func(t *testing.T) {
		c, session := newWatermarkTestConn(t, false)
		c.do("enable", "ENABLE CONDSTORE")
		c.do("select", "SELECT INBOX")
		session.watermark.Store(3)

		resp := c.do("fetch", "FETCH 1:2 (FLAGS MODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH with MODSEQ items up to the watermark = %q, want no HIGHESTMODSEQ", resp)
		}
	})

	t.Run("no watermark", func(t *testing.T) {
		c, _ := newWatermarkTestConn(t, false)
		c.do("enable", "ENABLE CONDSTORE")
		c.do("select", "SELECT INBOX")

		resp := c.do("fetch", "FETCH 1:2 (FLAGS MODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH with HighestModSeq 0 = %q, want no HIGHESTMODSEQ", resp)
		}
	})

	t.Run("not CONDSTORE-aware", func(t *testing.T) {
		c, session := newWatermarkTestConn(t, false)
		c.do("select", "SELECT INBOX")
		session.watermark.Store(1)

		resp := c.do("store", `STORE 1 +FLAGS (\Flagged)`)
		if strings.Contains(resp, "MODSEQ") || len(highestModSeqLines(resp)) != 0 {
			t.Errorf("STORE without CONDSTORE = %q, want neither MODSEQ nor HIGHESTMODSEQ", resp)
		}
	})

	t.Run("selected with NOMODSEQ", func(t *testing.T) {
		c, session := newWatermarkTestConn(t, true)
		c.do("enable", "ENABLE CONDSTORE")
		if resp := c.do("select", "SELECT INBOX"); !strings.Contains(resp, "[NOMODSEQ]") {
			t.Fatalf("SELECT = %q, want NOMODSEQ", resp)
		}
		session.watermark.Store(1)

		resp := c.do("fetch", "FETCH 1:2 (FLAGS MODSEQ)")
		if lines := highestModSeqLines(resp); len(lines) != 0 {
			t.Errorf("FETCH on a NOMODSEQ mailbox = %q, want no HIGHESTMODSEQ", resp)
		}
	})
}
