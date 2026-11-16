package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type notifyPartialByeWatermarkSession struct {
	imapserver.Session
	conn *imapserver.Conn
}

func (s *notifyPartialByeWatermarkSession) HighestModSeq() uint64 { return 3 }

func (s *notifyPartialByeWatermarkSession) Select(ctx context.Context, box string, opts *imap.SelectOptions) (*imap.SelectData, error) {
	d, e := s.Session.Select(ctx, box, opts)
	if e == nil {
		d.HighestModSeq = 3
	}
	return d, e
}

func (s *notifyPartialByeWatermarkSession) SetNotify(ctx context.Context, opts *imap.NotifyOptions, w *imapserver.UpdateWriter) error {
	if err := w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
		return err
	}
	return fmt.Errorf("backend wrapper: %w", &imap.Error{Type: imap.StatusResponseTypeBye, Text: "Backend lost mailbox after partial notification"})
}

func (s *notifyPartialByeWatermarkSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}
func TestNotifyByeAfterPartialFetchFlushesWatermark(t *testing.T) {
	for _, alreadyEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_enabled=%v", alreadyEnabled), func(t *testing.T) {
			mem := imapmemserver.New()
			u := imapmemserver.NewUser("user", "pass")
			if e := u.Create(context.Background(), "INBOX", nil); e != nil {
				t.Fatal(e)
			}
			mem.AddUser(u)
			sessions := make(chan *notifyPartialByeWatermarkSession, 1)
			srv := imapserver.New(&imapserver.Options{
				NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					session := &notifyPartialByeWatermarkSession{Session: mem.NewSession(), conn: conn}
					sessions <- session
					return session, nil, nil
				},
				Caps: imap.CapSet{
					imap.CapIMAP4rev1: {},
					imap.CapCondStore: {},
					imap.CapNotify:    {},
					imap.CapEnable:    {},
				},
				InsecureAuth: true,
				Logger:       log.New(io.Discard, "", 0),
			})
			t.Cleanup(func() { srv.Close() })
			ln, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			go srv.Serve(ln)
			conn, e := net.Dial("tcp", ln.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { conn.Close() })
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
			if _, e := c.br.ReadString('\n'); e != nil {
				t.Fatal(e)
			}
			session := <-sessions
			c.do("l", "LOGIN user pass")
			if alreadyEnabled {
				c.do("e", "ENABLE CONDSTORE")
			}
			c.appendMessage("body")
			c.do("s", "SELECT INBOX")
			if got := session.conn.CondStoreEnabled(); got != alreadyEnabled {
				t.Fatalf("fixture CONDSTORE = %v, want %v", got, alreadyEnabled)
			}
			// No NOTIFY pump was installed. The failing backend writes the update
			// itself, then discovers that it cannot keep the session alive. Its safe
			// watermark remains below the update because lower changes are delayed.
			if _, e := fmt.Fprint(conn, "n NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge FlagChange))\r\n"); e != nil {
				t.Fatal(e)
			}
			raw, e := io.ReadAll(c.br)
			if e != nil {
				t.Fatal(e)
			}
			resp := string(raw)
			bye := strings.Index(resp, "* BYE")
			if bye < 0 {
				t.Fatalf("missing BYE: %q", resp)
			}
			modSeq := strings.Index(resp[:bye], "MODSEQ (10)")
			if modSeq < 0 {
				t.Fatalf("fixture did not write MODSEQ: %q", resp)
			}
			watermark := strings.Index(resp[:bye], "* OK [HIGHESTMODSEQ 3]")
			if watermark < modSeq {
				t.Fatalf("lost compensating watermark before BYE after partial MODSEQ FETCH: %q", resp)
			}
			if got := session.conn.CondStoreEnabled(); got != alreadyEnabled {
				t.Errorf("CONDSTORE after BYE = %v, want pre-command state %v", got, alreadyEnabled)
			}
		})
	}
}
