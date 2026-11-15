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

// condStoreFilterSession can filter CONDSTORE out before SELECT and offer it
// later. With the extension filtered out, the backend omits the optional
// SelectData.HighestModSeq; NoMod represents a mailbox without mod-sequences.
type condStoreFilterSession struct {
	imapserver.Session
	condstore atomic.Bool
}

func (s *condStoreFilterSession) GetCapabilities() imap.CapSet {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}}
	if s.condstore.Load() {
		caps[imap.CapCondStore] = struct{}{}
	}
	return caps
}

func (s *condStoreFilterSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := s.Session.Select(ctx, mailbox, options)
	if data != nil && (!s.condstore.Load() || mailbox == "NoMod") {
		data.HighestModSeq = 0
	}
	return data, err
}

func (s *condStoreFilterSession) HighestModSeq() uint64 { return firstEnableHighestModSeq }

func newCondStoreFilterTestConn(t *testing.T) (*storeTestConn, *condStoreFilterSession) {
	t.Helper()
	user := imapmemserver.NewUser("user", "pass")
	for _, mailbox := range []string{"INBOX", "NoMod"} {
		if err := user.Create(context.Background(), mailbox, nil); err != nil {
			t.Fatalf("Create(%q): %v", mailbox, err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	session := &condStoreFilterSession{Session: mem.NewSession()}
	session.condstore.Store(true)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return session, nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}},
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
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatalf("Read greeting: %v", err)
	}
	if resp := c.do("login", "LOGIN user pass"); !strings.Contains(resp, "login OK") {
		t.Fatalf("LOGIN: %q", resp)
	}
	c.appendMessage("append")
	return c, session
}

// RFC 7162 §3.1 exempts the first CONDSTORE enabling command from returning
// HIGHESTMODSEQ only if SELECT actually sent NOMODSEQ for the selected mailbox.
// A zero backend value while the capability is filtered out sends neither
// response code and must not be mistaken for that exemption.
func TestFirstCondStoreEnablingAfterCapabilityFilter(t *testing.T) {
	for _, tc := range []struct {
		name, mailbox string
		condstore     bool
		selectCode    string
		reselect      bool
		wantHighest   bool
	}{
		{"filtered out at SELECT", "INBOX", false, "", false, true},
		{"mod-sequences at SELECT", "INBOX", true, "HIGHESTMODSEQ", false, true},
		{"NOMODSEQ at SELECT", "NoMod", true, "NOMODSEQ", false, false},
		{"NOMODSEQ belongs to previous mailbox", "NoMod", true, "NOMODSEQ", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, session := newCondStoreFilterTestConn(t)
			session.condstore.Store(tc.condstore)
			resp := c.do("select", "SELECT "+tc.mailbox)
			if !strings.Contains(resp, "select OK") {
				t.Fatalf("SELECT: %q", resp)
			}
			if tc.selectCode == "" {
				if strings.Contains(resp, "NOMODSEQ") || strings.Contains(resp, "HIGHESTMODSEQ") {
					t.Fatalf("filtered CONDSTORE sent a response code: %q", resp)
				}
			} else if !strings.Contains(resp, "["+tc.selectCode+" ") && !strings.Contains(resp, "["+tc.selectCode+"]") {
				t.Fatalf("SELECT: %q, want %s", resp, tc.selectCode)
			}

			session.condstore.Store(true)
			if tc.reselect {
				if resp := c.do("reselect", "SELECT INBOX"); !strings.Contains(resp, "reselect OK") {
					t.Fatalf("SELECT INBOX: %q", resp)
				}
			}
			resp = c.do("first", "STATUS INBOX (HIGHESTMODSEQ)")
			if !strings.Contains(resp, "first OK") {
				t.Fatalf("STATUS: %q", resp)
			}
			lines := highestModSeqLines(resp)
			if tc.wantHighest {
				if len(lines) != 1 || !strings.HasPrefix(lines[0], "* OK [HIGHESTMODSEQ 4242] ") {
					t.Fatalf("first enabling command: %q, want selected-mailbox HIGHESTMODSEQ 4242", resp)
				}
			} else if len(lines) != 0 {
				t.Fatalf("first enabling command on NOMODSEQ mailbox: %q, want no HIGHESTMODSEQ", resp)
			}
			if resp := c.do("second", "STATUS INBOX (HIGHESTMODSEQ)"); len(highestModSeqLines(resp)) != 0 {
				t.Fatalf("second enabling command repeated HIGHESTMODSEQ: %q", resp)
			}
		})
	}
}
