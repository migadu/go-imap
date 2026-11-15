package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type closeBoundarySession struct {
	imapserver.Session
	inboxWatermark atomic.Uint64
	fired          atomic.Bool
	trigger        chan struct{}
	written        chan struct{}
}

func (s *closeBoundarySession) HighestModSeq() uint64 {
	return s.inboxWatermark.Load()
}

func (s *closeBoundarySession) Expunge(ctx context.Context, w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	// Simulate RFC 7162 §3.2.8: silent expunge on CLOSE advances the mailbox's
	// modseq, which MUST NOT be reported as CLOSE's HIGHESTMODSEQ; any
	// compensating watermark must reflect the pre-CLOSE safe watermark (3).
	if err := s.Session.Expunge(ctx, w, uids); err != nil {
		return err
	}
	s.inboxWatermark.Store(99)
	return nil
}

func (s *closeBoundarySession) Unselect(ctx context.Context) error {
	s.inboxWatermark.Store(0)
	return s.Session.Unselect(ctx)
}

func (s *closeBoundarySession) Unauthenticate(ctx context.Context) error {
	s.inboxWatermark.Store(0)
	return nil
}

func (s *closeBoundarySession) SetNotify(context.Context, *imap.NotifyOptions, *imapserver.UpdateWriter) error {
	return nil
}

func (s *closeBoundarySession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if !s.fired.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
		case <-stop:
		}
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-stop:
		return nil
	case <-s.trigger:
	}
	if err := w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
		return err
	}
	close(s.written)
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}

func newCloseBoundaryTestConn(t *testing.T) (*storeTestConn, *closeBoundarySession) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	for _, box := range []string{"INBOX", "Archive"} {
		if err := user.Create(context.Background(), box, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)

	sessions := make(chan *closeBoundarySession, 1)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := &closeBoundarySession{
				Session: mem.NewSession(),
				trigger: make(chan struct{}),
				written: make(chan struct{}),
			}
			sessions <- s
			return s, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:      {},
			imap.CapCondStore:      {},
			imap.CapQResync:        {},
			imap.CapEnable:         {},
			imap.CapNotify:         {},
			imap.CapUnauthenticate: {},
		},
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn), addr: ln.Addr().String()}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	s := <-sessions
	c.do("login", "LOGIN user pass")
	c.appendMessage("append1")
	c.appendMessage("append2")
	c.do("enable", "ENABLE QRESYNC")
	c.do("select", "SELECT INBOX")
	c.do("del", `STORE 2 +FLAGS.SILENT (\Deleted)`)
	// Safe watermark is 3 (expunge 4 is held back by SELECTED-DELAYED).
	s.inboxWatermark.Store(3)
	c.do("notify", "NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge FlagChange))")
	return c, s
}

func TestClosingCommandsEmitDelayedWatermark(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cmd        string
		wantTagPfx string
		beforeLine string
	}{
		{"UNSELECT", "UNSELECT", "end OK", ""},
		{"CLOSE", "CLOSE", "end OK", ""},
		{"SELECT_Archive", "SELECT Archive", "end OK", "* OK [CLOSED]"},
		{"SELECT_Missing", "SELECT Missing", "end NO", "* OK [CLOSED]"},
		{"UNAUTHENTICATE", "UNAUTHENTICATE", "end OK", ""},
		{"LOGOUT", "LOGOUT", "end OK", "* BYE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := newCloseBoundaryTestConn(t)

			close(s.trigger)
			select {
			case <-s.written:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for unsolicited FETCH")
			}

			resp := c.do("end", tc.cmd)
			if !strings.Contains(resp, "MODSEQ (10)") {
				t.Fatalf("missing unsolicited MODSEQ (10) in span:\n%s", resp)
			}
			if !strings.Contains(resp, tc.wantTagPfx) {
				t.Fatalf("unexpected completion for %s:\n%s", tc.cmd, resp)
			}

			bound := strings.Index(resp, tc.wantTagPfx)
			if tc.beforeLine != "" {
				closedIdx := strings.Index(resp, tc.beforeLine)
				if closedIdx < 0 {
					t.Fatalf("missing %q in %s response:\n%s", tc.beforeLine, tc.cmd, resp)
				}
				bound = closedIdx
			}

			preCloseSpan := resp[:bound]
			modIdx := strings.Index(preCloseSpan, "MODSEQ (10)")
			wmIdx := strings.Index(preCloseSpan, "* OK [HIGHESTMODSEQ 3]")
			if wmIdx < 0 || wmIdx < modIdx {
				t.Fatalf("%s leaked MODSEQ (10) across mailbox close without compensating * OK [HIGHESTMODSEQ 3] (RFC 7162 §3.2, §3.2.8, §3.2.11, §6):\n%s", tc.cmd, resp)
			}
		})
	}
}

type notifyHighestModSeqSession struct {
	imapserver.Session
}

func (s *notifyHighestModSeqSession) HighestModSeq() uint64 {
	return firstEnableHighestModSeq
}

func (s *notifyHighestModSeqSession) SetNotify(ctx context.Context, options *imap.NotifyOptions, w *imapserver.UpdateWriter) error {
	return s.Session.(imapserver.SessionNotify).SetNotify(ctx, options, w)
}

func (s *notifyHighestModSeqSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	return s.Session.(imapserver.SessionNotify).NotifyPoll(ctx, w, stop)
}

func newNotifyEnableTestConn(t *testing.T, enableNotifyCap bool) *storeTestConn {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)

	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapCondStore: {},
		imap.CapEnable:    {},
	}
	if enableNotifyCap {
		caps[imap.CapNotify] = struct{}{}
	}

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &notifyHighestModSeqSession{Session: mem.NewSession()}, nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn), addr: ln.Addr().String()}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	c.do("login", "LOGIN user pass")
	c.appendMessage("append1")
	return c
}

func TestNotifyMessageNewModSeqFirstEnableAndRefusals(t *testing.T) {
	t.Run("first_enabling_NOTIFY_SET_returns_HIGHESTMODSEQ", func(t *testing.T) {
		c := newNotifyEnableTestConn(t, true)
		c.do("select", "SELECT INBOX")

		resp := c.do("notify", "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge FlagChange))")
		if !strings.Contains(resp, "notify OK") {
			t.Fatalf("NOTIFY failed: %q", resp)
		}
		lines := highestModSeqLines(resp)
		if len(lines) != 1 || !strings.Contains(lines[0], "HIGHESTMODSEQ 4242]") {
			t.Fatalf("NOTIFY SET with MessageNew (... MODSEQ ...) = %q, want 1 * OK [HIGHESTMODSEQ 4242] line (RFC 7162 §3.1)", resp)
		}

		// Subsequent enabling command must not duplicate the first-enable response.
		if resp2 := c.do("fetch", "FETCH 1 (MODSEQ)"); len(highestModSeqLines(resp2)) != 0 {
			t.Fatalf("second enabling command duplicated * OK [HIGHESTMODSEQ]: %q", resp2)
		}
	})

	t.Run("unsupported_NOTIFY_does_not_enable_CONDSTORE", func(t *testing.T) {
		c := newNotifyEnableTestConn(t, false)
		c.do("select", "SELECT INBOX")

		resp := c.do("notify", "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge))")
		if !strings.Contains(resp, "notify NO") {
			t.Fatalf("expected NO for unsupported NOTIFY, got: %q", resp)
		}
		if len(highestModSeqLines(resp)) != 0 {
			t.Fatalf("unsupported NOTIFY emitted HIGHESTMODSEQ before NO: %q", resp)
		}

		storeResp := c.do("store", `STORE 1 +FLAGS (\Seen)`)
		if strings.Contains(storeResp, "MODSEQ") {
			t.Fatalf("unsupported NOTIFY enabled CONDSTORE and leaked unrequested MODSEQ on STORE (RFC 7162 §3.2): %q", storeResp)
		}
	})

	t.Run("BADEVENT_NOTIFY_still_announces_CONDSTORE", func(t *testing.T) {
		c := newNotifyEnableTestConn(t, true)
		c.do("select", "SELECT INBOX")

		resp := c.do("notify", "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge UnsupportedEventX))")
		if !strings.Contains(resp, "notify NO [BADEVENT") {
			t.Fatalf("expected NO [BADEVENT], got: %q", resp)
		}
		if len(highestModSeqLines(resp)) != 1 {
			t.Fatalf("valid NOTIFY answered NO [BADEVENT] missed first-enable HIGHESTMODSEQ: %q", resp)
		}

		storeResp := c.do("store", `STORE 1 +FLAGS (\Seen)`)
		if !strings.Contains(storeResp, "MODSEQ (") {
			t.Fatalf("STORE after valid NOTIFY answered NO [BADEVENT] omitted mandatory MODSEQ: %q", storeResp)
		}

		fetchResp := c.do("fetch", "FETCH 1 (MODSEQ)")
		if lines := highestModSeqLines(fetchResp); len(lines) != 0 {
			t.Fatalf("subsequent enabling FETCH repeated first-enable HIGHESTMODSEQ: %q", fetchResp)
		}
	})
}

// A valid enabling command announces the client's willingness to receive
// MODSEQ even when the operation fails with NO (RFC 7162 section 3.1).
func TestUnsuccessfulEnablingCommandAnnouncesCondStore(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setupCmd  string
		failedCmd string
	}{
		{
			name:      "STATUS_NonExistent_HIGHESTMODSEQ",
			setupCmd:  "SELECT INBOX",
			failedCmd: "STATUS DoesNotExist (HIGHESTMODSEQ)",
		},
		{
			name:      "SEARCH_BADCHARSET_MODSEQ",
			setupCmd:  "SELECT INBOX",
			failedCmd: "SEARCH CHARSET ISO-8859-1 MODSEQ 1",
		},
		{
			name:      "EXAMINE_STORE_UNCHANGEDSINCE",
			setupCmd:  "EXAMINE INBOX",
			failedCmd: `STORE 1 (UNCHANGEDSINCE 1) +FLAGS (\Seen)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFirstEnableTestConn(t, false)
			if resp := c.do("sel", tc.setupCmd); !strings.Contains(resp, "sel OK") {
				t.Fatalf("%s failed: %q", tc.setupCmd, resp)
			}

			resp := c.do("fail", tc.failedCmd)
			if !strings.Contains(resp, "fail NO") {
				t.Fatalf("%s = %q, want tagged NO", tc.failedCmd, resp)
			}
			if lines := highestModSeqLines(resp); len(lines) != 1 {
				t.Errorf("valid enabling command %q answered NO missed first-enable HIGHESTMODSEQ: %q", tc.failedCmd, resp)
			}

			if tc.setupCmd == "EXAMINE INBOX" {
				c.do("reselect", "SELECT INBOX")
			}
			storeResp := c.do("store", `STORE 1 +FLAGS (\Flagged)`)
			if !strings.Contains(storeResp, "store OK") || !strings.Contains(storeResp, "MODSEQ (") {
				t.Errorf("STORE after valid enabling NO omitted mandatory MODSEQ: %q", storeResp)
			}

			// Awareness already began with the valid unsuccessful command.
			resp = c.do("real", "STATUS INBOX (HIGHESTMODSEQ)")
			if !strings.Contains(resp, "real OK") {
				t.Fatalf("STATUS INBOX (HIGHESTMODSEQ) = %q, want OK", resp)
			}
			if lines := highestModSeqLines(resp); len(lines) != 0 {
				t.Errorf("subsequent enabling command after valid NO %q repeated first-enable HIGHESTMODSEQ: %q", tc.failedCmd, resp)
			}
		})
	}

	t.Run("SELECT_NonExistent_CONDSTORE", func(t *testing.T) {
		c := newStoreTestConn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}}, 1)
		if resp := c.do("close", "CLOSE"); !strings.Contains(resp, "close OK") {
			t.Fatalf("CLOSE: %q", resp)
		}
		if resp := c.do("selbad", "SELECT NonExistent (CONDSTORE)"); !strings.Contains(resp, "selbad NO") {
			t.Fatalf("SELECT NonExistent (CONDSTORE) = %q, want NO", resp)
		}
		if !c.srvConn.CondStoreEnabled() {
			t.Fatal("valid SELECT NonExistent (CONDSTORE) answered NO did not announce CONDSTORE")
		}
		c.do("select", "SELECT INBOX")
		if resp := c.do("store", `STORE 1 +FLAGS (\Flagged)`); !strings.Contains(resp, "MODSEQ (") {
			t.Fatalf("STORE after valid SELECT answered NO omitted mandatory MODSEQ: %q", resp)
		}
	})
}

func TestPreAuthTooBigDoesNotEnableCondStoreAcrossLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		preCmd string
	}{
		{
			name:   "LIST_RETURN_STATUS_HIGHESTMODSEQ",
			preCmd: `LIST "" "*" RETURN (STATUS (HIGHESTMODSEQ) METADATA ({4097}`,
		},
		{
			name:   "SEARCH_MODSEQ",
			preCmd: "SEARCH MODSEQ 1 TEXT {4097}",
		},
		{
			name:   "FETCH_MODSEQ",
			preCmd: "FETCH 1 (MODSEQ BODY[HEADER.FIELDS ({4097}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memUser := imapmemserver.NewUser("user", "pass")
			if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
				t.Fatal(err)
			}
			memServer := imapmemserver.New()
			memServer.AddUser(memUser)

			var (
				mu      sync.Mutex
				srvConn *imapserver.Conn
			)
			srv := imapserver.New(&imapserver.Options{
				NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					mu.Lock()
					srvConn = conn
					mu.Unlock()
					return &highestModSeqSession{Session: memServer.NewSession()}, nil, nil
				},
				Caps: imap.CapSet{
					imap.CapIMAP4rev1:  {},
					imap.CapCondStore:  {},
					imap.CapListStatus: {},
					imap.CapMetadata:   {},
				},
				InsecureAuth: true,
			})
			t.Cleanup(func() { srv.Close() })

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(ln)

			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}

			c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
			if _, err := c.br.ReadString('\n'); err != nil {
				t.Fatal(err)
			}

			resp := c.do("pre", tc.preCmd)
			if !strings.Contains(resp, "pre NO [TOOBIG]") {
				t.Fatalf("pre-auth command = %q, want NO [TOOBIG]", resp)
			}

			if resp := c.do("login", "LOGIN user pass"); !strings.Contains(resp, "login OK") {
				t.Fatalf("LOGIN: %q", resp)
			}
			c.appendMessage("msg1")
			if resp := c.do("sel", "SELECT INBOX"); !strings.Contains(resp, "sel OK") {
				t.Fatalf("SELECT: %q", resp)
			}

			mu.Lock()
			enabled := srvConn.CondStoreEnabled()
			mu.Unlock()
			if enabled {
				t.Errorf("CondStoreEnabled() = true after LOGIN because pre-auth %s polluted connection state", tc.name)
			}

			resp = c.do("store", `STORE 1 +FLAGS (\Seen)`)
			if strings.Contains(resp, "MODSEQ (") {
				t.Errorf("STORE sent unrequested MODSEQ to a logged-in client that never enabled CONDSTORE: %q", resp)
			}
		})
	}
}

type midParseNotifySession struct {
	imapserver.Session
	trigger chan struct{}
	written chan struct{}
}

func (s *midParseNotifySession) SetNotify(context.Context, *imap.NotifyOptions, *imapserver.UpdateWriter) error {
	return nil
}

func (s *midParseNotifySession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return nil
	case <-stop:
		return nil
	case <-s.trigger:
	}
	if err := w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagFlagged}, 5); err != nil {
		return err
	}
	close(s.written)
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}

func TestNotifyPumpDoesNotSeeMidParseCondStore(t *testing.T) {
	t.Run("SEARCH_MODSEQ_literal_mid_parse", func(t *testing.T) {
		memUser := imapmemserver.NewUser("user", "pass")
		if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
			t.Fatal(err)
		}
		memServer := imapmemserver.New()
		memServer.AddUser(memUser)

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return memServer.NewSession(), nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1: {},
				imap.CapCondStore: {},
				imap.CapNotify:    {},
			},
			InsecureAuth: true,
		})
		t.Cleanup(func() { srv.Close() })

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln)

		dial := func() *storeTestConn {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			sc := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
			if _, err := sc.br.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			if resp := sc.do("login", "LOGIN user pass"); !strings.Contains(resp, "login OK") {
				t.Fatalf("LOGIN: %q", resp)
			}
			return sc
		}

		c1 := dial()
		c2 := dial()
		c1.appendMessage("msg1")
		c1.do("sel", "SELECT INBOX")
		c2.do("sel", "SELECT INBOX")
		c1.do("notify", "NOTIFY SET (SELECTED (MessageNew (UID FLAGS) MessageExpunge FlagChange))")

		if _, err := c1.conn.Write([]byte("bad1 SEARCH MODSEQ 1 TEXT {5}\r\n")); err != nil {
			t.Fatal(err)
		}
		cont, err := c1.br.ReadString('\n')
		if err != nil || !strings.HasPrefix(cont, "+ ") {
			t.Fatalf("expected continuation '+', got %q (err=%v)", cont, err)
		}

		c2.do("store", `STORE 1 +FLAGS (\Seen)`)

		unsolicited, err := c1.br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading unsolicited NOTIFY FETCH: %v", err)
		}

		if _, err := c1.conn.Write([]byte("hello (BOGUS\r\n")); err != nil {
			t.Fatal(err)
		}
		badResp := c1.readUntilTag("bad1")
		if !strings.Contains(badResp, "bad1 BAD") {
			t.Fatalf("expected bad1 BAD, got %q", badResp)
		}
		if strings.Contains(unsolicited, "MODSEQ (") {
			t.Fatalf("background NOTIFY pump leaked MODSEQ during mid-parse of a BAD command: %q", unsolicited)
		}
	})

	t.Run("SELECT_CONDSTORE_mid_parse", func(t *testing.T) {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser("user", "pass")
		if err := user.Create(context.Background(), "INBOX", nil); err != nil {
			t.Fatal(err)
		}
		mem.AddUser(user)

		var (
			mu      sync.Mutex
			srvConn *imapserver.Conn
		)
		sessions := make(chan *midParseNotifySession, 1)
		srv := imapserver.New(&imapserver.Options{
			NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				mu.Lock()
				srvConn = conn
				mu.Unlock()
				s := &midParseNotifySession{
					Session: mem.NewSession(),
					trigger: make(chan struct{}),
					written: make(chan struct{}),
				}
				sessions <- s
				return s, nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1: {},
				imap.CapCondStore: {},
				imap.CapQResync:   {},
				imap.CapEnable:    {},
				imap.CapNotify:    {},
			},
			InsecureAuth: true,
		})
		t.Cleanup(func() { srv.Close() })

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln)

		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}

		c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
		if _, err := c.br.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		sess := <-sessions

		c.do("login", "LOGIN user pass")
		c.appendMessage("msg1")
		c.do("sel", "SELECT INBOX")
		c.do("notify", "NOTIFY SET (SELECTED (MessageNew MessageExpunge FlagChange))")

		if _, err := fmt.Fprint(conn, "bad2 SELECT INBOX (CONDSTORE "); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		sc := srvConn
		mu.Unlock()
		deadline := time.Now().Add(2 * time.Second)
		for !sc.CondStoreEnabled() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for SELECT (CONDSTORE ...) to be parsed")
			}
			runtime.Gosched()
		}

		close(sess.trigger)
		<-sess.written

		if _, err := fmt.Fprint(conn, "QRESYNC (1 1))\r\n"); err != nil {
			t.Fatal(err)
		}
		resp := c.readUntilTag("bad2")
		if !strings.Contains(resp, "bad2 BAD") {
			t.Fatalf("expected bad2 BAD, got %q", resp)
		}
		if strings.Contains(resp, "MODSEQ") {
			t.Fatalf("non-CONDSTORE client received unsolicited MODSEQ during a BAD SELECT: %q", resp)
		}
	})
}
