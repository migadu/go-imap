package imapclient_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type gatedStatusBoundarySession struct {
	*closeSelectBoundarySession
	inStatus      chan struct{}
	releaseStatus chan struct{}
}

func (s *gatedStatusBoundarySession) Status(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	close(s.inStatus)
	<-s.releaseStatus
	return s.closeSelectBoundarySession.Status(ctx, mailbox, options)
}

func TestQResyncSelectPreservesPreClosedUpdates(t *testing.T) {
	t.Run("imapserver_STATUS_then_QRESYNC_SELECT_delivers_pre_CLOSED_Fetch_handler", func(t *testing.T) {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser(testUsername, testPassword)
		for _, box := range []string{"INBOX", "Archive"} {
			if err := user.Create(context.Background(), box, nil); err != nil {
				t.Fatal(err)
			}
		}
		mem.AddUser(user)

		var sess *gatedStatusBoundarySession
		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				base := &closeSelectBoundarySession{
					Session: mem.NewSession(),
					trigger: make(chan struct{}),
					written: make(chan struct{}),
				}
				base.watermark.Store(3)
				sess = &gatedStatusBoundarySession{
					closeSelectBoundarySession: base,
					inStatus:                   make(chan struct{}),
					releaseStatus:              make(chan struct{}),
				}
				return sess, nil, nil
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
		t.Cleanup(func() { ln.Close() })
		go srv.Serve(ln)

		fetchCh := make(chan imap.UID, 4)
		c, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Fetch: func(msg *imapclient.FetchMessageData) {
					if buf, err := msg.Collect(); err == nil {
						fetchCh <- buf.UID
					}
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })

		if err := c.Login(testUsername, testPassword).Wait(); err != nil {
			t.Fatal(err)
		}
		app := c.Append("INBOX", 4, nil)
		_, _ = app.Write([]byte("test"))
		_ = app.Close()
		if _, err := app.Wait(); err != nil {
			t.Fatal(err)
		}

		if _, err := c.Enable(imap.CapQResync).Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		notifyCmd, err := c.Notify(&imap.NotifyOptions{
			Items: []imap.NotifyItem{{
				MailboxSpec: imap.NotifyMailboxSpecSelectedDelayed,
				Events:      []imap.NotifyEvent{imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge, imap.NotifyEventFlagChange},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := notifyCmd.Wait(); err != nil {
			t.Fatal(err)
		}

		statusCmd := c.Status("Archive", &imap.StatusOptions{NumMessages: true})
		<-sess.inStatus
		selectCmd := c.Select("Archive", &imap.SelectOptions{
			QResync: &imap.QResyncData{UIDValidity: 2, ModSeq: 1},
		})

		close(sess.trigger)
		<-sess.written
		close(sess.releaseStatus)

		if _, err := statusCmd.Wait(); err != nil {
			t.Fatalf("STATUS: %v", err)
		}
		if _, err := selectCmd.Wait(); err != nil {
			t.Fatalf("SELECT Archive: %v", err)
		}

		select {
		case uid := <-fetchCh:
			if uid != 1 {
				t.Errorf("UnilateralDataHandler.Fetch UID = %d, want 1", uid)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("UnilateralDataHandler.Fetch was never called; pre-CLOSED INBOX FETCH update was swallowed by pending QRESYNC SelectCommand and wiped on [CLOSED]")
		}
	})

	t.Run("standalone_QRESYNC_SELECT_delivers_pre_CLOSED_Exists_Fetch_and_Vanished_count", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 ENABLE CONDSTORE QRESYNC] ready\r\n")
			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* ENABLED QRESYNC CONDSTORE\r\n%s OK enabled\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			_, _ = fmt.Fprintf(sConn, "* 5 EXISTS\r\n* OK [UIDVALIDITY 10] ok\r\n* OK [HIGHESTMODSEQ 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag2)

			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 6 EXISTS\r\n"+
					"* 1 FETCH (UID 101 FLAGS (\\Seen) MODSEQ (2))\r\n"+
					"* VANISHED 101:102\r\n"+
					"* OK [CLOSED] Previous mailbox is now closed\r\n"+
					"* 0 EXISTS\r\n"+
					"* OK [UIDVALIDITY 20] ok\r\n"+
					"* OK [HIGHESTMODSEQ 50] ok\r\n"+
					"%s OK [READ-WRITE] selected\r\n", tag3)
		}()

		var (
			c                *imapclient.Client
			unilateralExists atomic.Uint32
			vanishedMboxCnt  atomic.Uint32
			fetchCh          = make(chan imap.UID, 2)
		)
		c = imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Mailbox: func(data *imapclient.UnilateralDataMailbox) {
					if data.NumMessages != nil {
						unilateralExists.Store(*data.NumMessages)
					}
				},
				Fetch: func(msg *imapclient.FetchMessageData) {
					if buf, err := msg.Collect(); err == nil {
						fetchCh <- buf.UID
					}
				},
				Vanished: func(*imap.VanishedData) {
					if mbox := c.Mailbox(); mbox != nil {
						vanishedMboxCnt.Store(mbox.NumMessages)
					}
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		_, _ = c.Enable(imap.CapQResync).Wait()
		_, _ = c.Select("INBOX", nil).Wait()
		if _, err := c.Select("Archive", &imap.SelectOptions{
			QResync: &imap.QResyncData{UIDValidity: 20, ModSeq: 1},
		}).Wait(); err != nil {
			t.Fatal(err)
		}
		if got := unilateralExists.Load(); got != 6 {
			t.Errorf("pre-CLOSED UnilateralDataHandler.Mailbox NumMessages = %d, want 6", got)
		}
		if got := vanishedMboxCnt.Load(); got != 4 {
			t.Errorf("pre-CLOSED UnilateralDataHandler.Vanished saw INBOX NumMessages = %d, want 4 (6 - 2)", got)
		}
		select {
		case <-fetchCh:
		case <-time.After(500 * time.Millisecond):
			t.Errorf("pre-CLOSED UnilateralDataHandler.Fetch was dropped")
		}
	})
}

func TestSelectedMailboxMultiSearchAndAppendBeforeSelect(t *testing.T) {
	t.Run("MultiSearch_on_selected_mailbox_before_BAD_SELECT_preserves_EXISTS", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 ESEARCH MULTISEARCH] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 2 EXISTS\r\n"+
					"* ESEARCH (TAG %q MAILBOX INBOX UIDVALIDITY 1) UID ALL 1:2\r\n"+
					"%s OK ESEARCH completed\r\n"+
					"%s BAD [CLIENTBUG] invalid SELECT\r\n", tag2, tag2, tag3)
		}()

		var unilateralExists atomic.Uint32
		c := imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Mailbox: func(data *imapclient.UnilateralDataMailbox) {
					if data.NumMessages != nil {
						unilateralExists.Store(*data.NumMessages)
					}
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		msCmd := c.MultiSearch(nil, &imap.SearchCriteria{}, nil)
		selCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		if _, err := msCmd.Wait(); err != nil {
			t.Fatalf("MultiSearch: %v", err)
		}
		if _, err := selCmd.Wait(); err == nil {
			t.Fatal("expected BAD from SELECT")
		}
		if got := c.Mailbox().NumMessages; got != 2 {
			t.Errorf("Mailbox().NumMessages = %d, want 2", got)
		}
		if got := unilateralExists.Load(); got != 2 {
			t.Errorf("UnilateralDataHandler.Mailbox NumMessages = %d, want 2", got)
		}
	})

	t.Run("Append_to_selected_mailbox_before_BAD_SELECT_preserves_EXISTS", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 LITERAL+] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 0 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n') // T2 APPEND INBOX {4+}\r\n
			tag2, _, _ := strings.Cut(line2, " ")
			_, _ = br.ReadString('\n') // test\r\n
			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 1 EXISTS\r\n"+
					"%s OK APPEND completed\r\n"+
					"%s BAD [CLIENTBUG] invalid SELECT\r\n", tag2, tag3)
		}()

		var unilateralExists atomic.Uint32
		c := imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Mailbox: func(data *imapclient.UnilateralDataMailbox) {
					if data.NumMessages != nil {
						unilateralExists.Store(*data.NumMessages)
					}
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		appCmd := c.Append("inbox", 4, nil)
		_, _ = appCmd.Write([]byte("test"))
		_ = appCmd.Close()
		selCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		if _, err := appCmd.Wait(); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if _, err := selCmd.Wait(); err == nil {
			t.Fatal("expected BAD from SELECT")
		}
		if got := c.Mailbox().NumMessages; got != 1 {
			t.Errorf("Mailbox().NumMessages = %d, want 1", got)
		}
		if got := unilateralExists.Load(); got != 1 {
			t.Errorf("UnilateralDataHandler.Mailbox NumMessages = %d, want 1", got)
		}
	})
}
