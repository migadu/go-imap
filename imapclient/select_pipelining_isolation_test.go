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

type closeSelectBoundarySession struct {
	imapserver.Session
	watermark atomic.Uint64
	fired     atomic.Bool
	trigger   chan struct{}
	written   chan struct{}
}

func (s *closeSelectBoundarySession) HighestModSeq() uint64 {
	return s.watermark.Load()
}

func (s *closeSelectBoundarySession) SetNotify(context.Context, *imap.NotifyOptions, *imapserver.UpdateWriter) error {
	return nil
}

func (s *closeSelectBoundarySession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
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

func TestPR254_PipelinedCloseBeforeSelectLeaksAndDropsData(t *testing.T) {
	t.Run("imapserver_CLOSE_then_SELECT_leaks_Modified_and_drops_Fetch_handler", func(t *testing.T) {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser(testUsername, testPassword)
		for _, box := range []string{"INBOX", "Archive"} {
			if err := user.Create(context.Background(), box, nil); err != nil {
				t.Fatal(err)
			}
		}
		mem.AddUser(user)

		var sess *closeSelectBoundarySession
		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				sess = &closeSelectBoundarySession{
					Session: mem.NewSession(),
					trigger: make(chan struct{}),
					written: make(chan struct{}),
				}
				sess.watermark.Store(3)
				return sess, nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1: {},
				imap.CapCondStore: {},
				imap.CapQResync:   {},
				imap.CapEnable:    {},
				imap.CapNotify:    {},
				imap.CapUnselect:  {},
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

		var unilateralFetchCount atomic.Int32
		c, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Fetch: func(*imapclient.FetchMessageData) {
					unilateralFetchCount.Add(1)
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
				Events: []imap.NotifyEvent{
					imap.NotifyEventMessageNew,
					imap.NotifyEventMessageExpunge,
					imap.NotifyEventFlagChange,
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := notifyCmd.Wait(); err != nil {
			t.Fatal(err)
		}

		// Pipeline STATUS Archive (during which NotifyPoll emits a flag update for
		// INBOX) + CLOSE + SELECT Archive against real imapserver.
		statusCmd := c.Status("Archive", &imap.StatusOptions{NumMessages: true})
		closeCmd := c.UnselectAndExpunge()
		selectCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		close(sess.trigger)
		<-sess.written

		if _, err := statusCmd.Wait(); err != nil {
			t.Fatalf("STATUS: %v", err)
		}
		if err := closeCmd.Wait(); err != nil {
			t.Fatalf("CLOSE: %v", err)
		}
		data, err := selectCmd.Wait()
		if err != nil {
			t.Fatalf("SELECT Archive: %v", err)
		}
		if len(data.Modified) != 0 {
			t.Errorf("Select(Archive).Modified = %+v, want empty (leaked from INBOX across CLOSE)", data.Modified)
		}
	})

	t.Run("deterministic_CLOSE_then_SELECT_leaks_HighestModSeq_and_Modified", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() {
			cConn.Close()
			sConn.Close()
		})
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE UNSELECT] ready\r\n")

			// 1. SELECT INBOX (without CONDSTORE)
			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 1 EXISTS\r\n"+
					"* OK [UIDVALIDITY 1] ok\r\n"+
					"* OK [UIDNEXT 99] ok\r\n"+
					"* FLAGS (\\Seen)\r\n"+
					"* OK [PERMANENTFLAGS (\\Seen \\*)] ok\r\n"+
					"%s OK [READ-WRITE] SELECT completed\r\n", tag1)

			// 2. Client pipelines:
			//    tag2 FETCH 1 (MODSEQ)   (first CONDSTORE-enabling command on INBOX -> * OK [HIGHESTMODSEQ 4242])
			//    tag3 CLOSE              (closes INBOX -> transitions to Authenticated, NO [CLOSED] code!)
			//    tag4 EXAMINE LegacyBox  (server omits optional HIGHESTMODSEQ/UIDNEXT/PERMANENTFLAGS)
			line2, _ := br.ReadString('\n')
			line3, _ := br.ReadString('\n')
			line4, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			tag3, _, _ := strings.Cut(line3, " ")
			tag4, _, _ := strings.Cut(line4, " ")

			_, _ = fmt.Fprintf(sConn,
				"* 1 FETCH (UID 1 FLAGS (\\Seen) MODSEQ (4242))\r\n"+
					"* OK [HIGHESTMODSEQ 4242] first enable on INBOX\r\n"+
					"%s OK FETCH completed\r\n"+
					"%s OK CLOSE completed\r\n"+
					"* 0 EXISTS\r\n"+
					"* OK [UIDVALIDITY 2] ok\r\n"+
					"* FLAGS (\\Seen)\r\n"+
					"%s OK [READ-ONLY] EXAMINE completed\r\n",
				tag2, tag3, tag4)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 2 * time.Second})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("Select(INBOX): %v", err)
		}

		fetchCmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{ModSeq: true})
		closeCmd := c.UnselectAndExpunge()
		selectCmd := c.Select("LegacyBox", &imap.SelectOptions{ReadOnly: true})

		if _, err := fetchCmd.Collect(); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if err := closeCmd.Wait(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		data, err := selectCmd.Wait()
		if err != nil {
			t.Fatalf("Select(LegacyBox): %v", err)
		}
		if data.HighestModSeq != 0 {
			t.Errorf("Select(LegacyBox).HighestModSeq = %d, want 0 (leaked 4242 from INBOX across CLOSE)", data.HighestModSeq)
		}
		if len(data.Modified) != 0 {
			t.Errorf("Select(LegacyBox).Modified = %+v, want empty (leaked from INBOX across CLOSE)", data.Modified)
		}
		if data.UIDNext != 0 {
			t.Errorf("Select(LegacyBox).UIDNext = %d, want 0 (leaked 99 from INBOX across CLOSE)", data.UIDNext)
		}
		if len(data.PermanentFlags) != 0 {
			t.Errorf("Select(LegacyBox).PermanentFlags = %v, want empty (leaked from INBOX across CLOSE)", data.PermanentFlags)
		}
	})
}

func TestPR254_PipelinedUIDStoreAndFetchAheadOfSelectSwallowedByClosedReset(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUsername, testPassword)
	for _, box := range []string{"INBOX", "Archive"} {
		if err := user.Create(context.Background(), box, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}},
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln)

	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
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
	appData, err := app.Wait()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait(); err != nil {
		t.Fatal(err)
	}

	// Pipeline UID STORE + UID FETCH ahead of SELECT Archive.
	storeCmd := c.Store(imap.UIDSetNum(appData.UID), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil)
	fetchCmd := c.Fetch(imap.UIDSetNum(appData.UID), &imap.FetchOptions{
		Flags:  true,
		ModSeq: true,
	})
	selectCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

	storeMsgs, err := storeCmd.Collect()
	if err != nil {
		t.Fatalf("Store.Collect: %v", err)
	}
	fetchMsgs, err := fetchCmd.Collect()
	if err != nil {
		t.Fatalf("Fetch.Collect: %v", err)
	}
	selectData, err := selectCmd.Wait()
	if err != nil {
		t.Fatalf("Select.Wait: %v", err)
	}

	if len(storeMsgs) != 1 {
		t.Errorf("UID STORE pipelined ahead of SELECT returned %d messages, want 1", len(storeMsgs))
	}
	if len(fetchMsgs) != 1 {
		t.Errorf("UID FETCH pipelined ahead of SELECT returned %d messages, want 1", len(fetchMsgs))
	}
	if len(selectData.Modified) != 0 {
		t.Errorf("Select(Archive).Modified = %+v, want empty", selectData.Modified)
	}
}

func TestPR254_CondStoreWithoutQResyncNoClosedAndPreClosedUnilateralUpdates(t *testing.T) {
	t.Run("condstore_server_without_QRESYNC_omits_CLOSED", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() {
			cConn.Close()
			sConn.Close()
		})
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			// RFC 7162 §3.2.11: CLOSED is only mandatory when QRESYNC is enabled;
			// a CONDSTORE-only server does not have to send [CLOSED].
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 1 EXISTS\r\n"+
					"* OK [UIDVALIDITY 1] ok\r\n"+
					"* OK [UIDNEXT 2] ok\r\n"+
					"* FLAGS (\\Seen)\r\n"+
					"* OK [PERMANENTFLAGS (\\Seen)] ok\r\n"+
					"* OK [HIGHESTMODSEQ 10] ok\r\n"+
					"%s OK [READ-WRITE] selected\r\n", tag1)

			// Client pipelines NOOP + SELECT Other (CONDSTORE).
			line2, _ := br.ReadString('\n')
			line3, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			tag3, _, _ := strings.Cut(line3, " ")

			_, _ = fmt.Fprintf(sConn,
				"* 1 FETCH (UID 1 FLAGS (\\Seen) MODSEQ (12))\r\n"+
					"* OK [HIGHESTMODSEQ 12] ok\r\n"+
					"%s OK NOOP completed\r\n"+
					"* 0 EXISTS\r\n"+
					"* OK [UIDVALIDITY 2] ok\r\n"+
					"* OK [UIDNEXT 1] ok\r\n"+
					"* FLAGS (\\Seen)\r\n"+
					"* OK [PERMANENTFLAGS (\\Seen)] ok\r\n"+
					"* OK [HIGHESTMODSEQ 77] ok\r\n"+
					"%s OK [READ-WRITE] selected\r\n", tag2, tag3)
		}()

		fetchCh := make(chan struct{}, 2)
		c := imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Fetch: func(*imapclient.FetchMessageData) {
					fetchCh <- struct{}{}
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait(); err != nil {
			t.Fatal(err)
		}

		noopCmd := c.Noop()
		selectCmd := c.Select("Other", &imap.SelectOptions{CondStore: true})

		if err := noopCmd.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := selectCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if len(data.Modified) != 0 {
			t.Errorf("Select(Other).Modified = %+v, want empty (leaked from INBOX on CONDSTORE-only server without CLOSED)", data.Modified)
		}
		select {
		case <-fetchCh:
		case <-time.After(500 * time.Millisecond):
			t.Errorf("UnilateralDataHandler.Fetch was not called for INBOX update during NOOP")
		}
	})

	t.Run("pipelined_NOOP_then_BAD_SELECT_loses_EXISTS_for_still_selected_mailbox", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() {
			cConn.Close()
			sConn.Close()
		})
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 1 EXISTS\r\n"+
					"* OK [UIDVALIDITY 1] ok\r\n"+
					"%s OK [READ-WRITE] selected\r\n", tag1)

			// Client pipelines NOOP + SELECT with unsupported QRESYNC (answered BAD,
			// so INBOX stays selected). During NOOP, * 5 EXISTS arrives for INBOX.
			line2, _ := br.ReadString('\n')
			line3, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 5 EXISTS\r\n"+
					"%s OK NOOP completed\r\n"+
					"%s BAD QRESYNC not enabled\r\n", tag2, tag3)
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

		noopCmd := c.Noop()
		selectCmd := c.Select("Other", &imap.SelectOptions{
			QResync: &imap.QResyncData{UIDValidity: 2, ModSeq: 1},
		})
		if err := noopCmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := selectCmd.Wait(); err == nil {
			t.Fatal("expected BAD from SELECT")
		}

		// BAD leaves INBOX selected; its NumMessages must reflect * 5 EXISTS.
		if mbox := c.Mailbox(); mbox == nil || mbox.NumMessages != 5 {
			t.Errorf("Mailbox() = %+v, want INBOX with NumMessages=5", mbox)
		}
		if got := unilateralExists.Load(); got != 5 {
			t.Errorf("UnilateralDataHandler.Mailbox NumMessages = %d, want 5", got)
		}
	})
}

type gatedStatusSelectSession struct {
	*closeSelectBoundarySession
	inStatus      chan struct{}
	releaseStatus chan struct{}
}

func (s *gatedStatusSelectSession) Status(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	close(s.inStatus)
	<-s.releaseStatus
	return s.closeSelectBoundarySession.Status(ctx, mailbox, options)
}

func TestSameMailboxReSelectWithClosedDeliversPreClosedUpdates(t *testing.T) {
	t.Run("imapserver_reselect_INBOX_with_QRESYNC_delivers_pre_CLOSED_Fetch", func(t *testing.T) {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser(testUsername, testPassword)
		for _, box := range []string{"INBOX", "Archive"} {
			if err := user.Create(context.Background(), box, nil); err != nil {
				t.Fatal(err)
			}
		}
		mem.AddUser(user)

		var sess *gatedStatusSelectSession
		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				base := &closeSelectBoundarySession{
					Session: mem.NewSession(),
					trigger: make(chan struct{}),
					written: make(chan struct{}),
				}
				base.watermark.Store(3)
				sess = &gatedStatusSelectSession{
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
		sel1, err := c.Select("INBOX", nil).Wait()
		if err != nil {
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
		reselectCmd := c.Select("INBOX", &imap.SelectOptions{
			ReadOnly: true,
			QResync:  &imap.QResyncData{UIDValidity: sel1.UIDValidity, ModSeq: sel1.HighestModSeq},
		})

		close(sess.trigger)
		<-sess.written
		close(sess.releaseStatus)

		if _, err := statusCmd.Wait(); err != nil {
			t.Fatalf("STATUS: %v", err)
		}
		if _, err := reselectCmd.Wait(); err != nil {
			t.Fatalf("EXAMINE INBOX: %v", err)
		}

		select {
		case uid := <-fetchCh:
			if uid != 1 {
				t.Errorf("UnilateralDataHandler.Fetch UID = %d, want 1", uid)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("pre-CLOSED INBOX FETCH update was dropped on same-mailbox re-SELECT")
		}
	})

	t.Run("server_omitting_CLOSED_on_same_mailbox_reselect_retains_SelectData", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE QRESYNC] ready\r\n")
			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n* OK [HIGHESTMODSEQ 5] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			// RFC 7162 §3.2.11: CLOSED is MAY when re-selecting the same mailbox.
			_, _ = fmt.Fprintf(sConn,
				"* 1 EXISTS\r\n"+
					"* OK [UIDVALIDITY 1] ok\r\n"+
					"* OK [HIGHESTMODSEQ 8] ok\r\n"+
					"* 1 FETCH (UID 1 FLAGS (\\Seen) MODSEQ (8))\r\n"+
					"%s OK [READ-ONLY] selected\r\n", tag2)
		}()

		var unilateralFetches atomic.Int32
		c := imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Fetch: func(*imapclient.FetchMessageData) {
					unilateralFetches.Add(1)
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := c.Select("INBOX", &imap.SelectOptions{
			ReadOnly: true,
			QResync:  &imap.QResyncData{UIDValidity: 1, ModSeq: 5},
		}).Wait()
		if err != nil {
			t.Fatalf("Select(INBOX): %v", err)
		}
		if len(data.Modified) != 1 || data.Modified[0].UID != 1 || data.Modified[0].ModSeq != 8 {
			t.Errorf("SelectData.Modified = %+v, want UID 1 ModSeq 8", data.Modified)
		}
		if got := unilateralFetches.Load(); got != 0 {
			t.Errorf("UnilateralDataHandler.Fetch calls = %d, want 0 when CLOSED was not sent", got)
		}
	})
}

func TestHandleListRespectsActiveSelectCmdAndCanonicalMailbox(t *testing.T) {
	t.Run("RENAME_then_SELECT_preserves_OLDNAME_LIST_notification", func(t *testing.T) {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser(testUsername, testPassword)
		for _, box := range []string{"INBOX", "Drafts"} {
			if err := user.Create(context.Background(), box, nil); err != nil {
				t.Fatal(err)
			}
		}
		mem.AddUser(user)

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return mem.NewSession(), nil, nil
			},
			Caps: imap.CapSet{
				imap.CapIMAP4rev1: {},
				imap.CapIMAP4rev2: {},
				imap.CapEnable:    {},
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

		var gotOldName atomic.Value
		c, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				List: func(data *imap.ListData) {
					if data.OldName != "" {
						gotOldName.Store(data.OldName + "->" + data.Mailbox)
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
		if _, err := c.Enable(imap.CapIMAP4rev2).Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		renCmd := c.Rename("Drafts", "Archive", nil)
		selCmd := c.Select("Archive", nil)

		if err := renCmd.Wait(); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		selData, err := selCmd.Wait()
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if gotOldName.Load() != "Drafts->Archive" {
			t.Errorf("UnilateralDataHandler.List OldName = %v, want Drafts->Archive (selData.List=%+v)", gotOldName.Load(), selData.List)
		}
	})

	t.Run("Select_lowercase_inbox_populates_SelectData_List", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 1 EXISTS\r\n"+
					"* OK [UIDVALIDITY 1] ok\r\n"+
					"* FLAGS (\\Seen)\r\n"+
					"* LIST (\\HasNoChildren) \"/\" INBOX\r\n"+
					"%s OK [READ-WRITE] SELECT completed\r\n", tag1)
		}()

		var leakedUnilateral atomic.Bool
		c := imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				List: func(*imap.ListData) {
					leakedUnilateral.Store(true)
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		data, err := c.Select("inbox", nil).Wait()
		if err != nil {
			t.Fatalf("Select(inbox): %v", err)
		}
		if data.List == nil || data.List.Mailbox != "INBOX" {
			t.Errorf("Select(inbox).List = %+v, want INBOX ListData (leakedUnilateral=%v)", data.List, leakedUnilateral.Load())
		}
	})
}

func TestSelectedMailboxCommandsBeforeSelectPreserveMailboxUpdates(t *testing.T) {
	t.Run("MultiSearch_Mailboxes_INBOX_before_BAD_SELECT", func(t *testing.T) {
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
		msCmd := c.MultiSearch(&imap.SearchSource{Mailboxes: []string{"INBOX"}}, &imap.SearchCriteria{}, nil)
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

	t.Run("Rename_parent_mailbox_before_BAD_SELECT", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 2 EXISTS\r\n"+
					"%s OK RENAME completed\r\n"+
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

		if _, err := c.Select("Work/2026", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		renCmd := c.Rename("Work", "Projects", nil)
		selCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		if err := renCmd.Wait(); err != nil {
			t.Fatalf("Rename: %v", err)
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

	t.Run("SetACL_on_selected_mailbox_before_BAD_SELECT", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 ACL] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n* OK [PERMANENTFLAGS (\\Seen \\*)] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* OK [PERMANENTFLAGS (\\Seen)] reduced rights\r\n"+
					"%s OK SETACL completed\r\n"+
					"%s BAD [CLIENTBUG] invalid SELECT\r\n", tag2, tag3)
		}()

		var unilateralPermFlags atomic.Bool
		c := imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 2 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Mailbox: func(data *imapclient.UnilateralDataMailbox) {
					if data.PermanentFlags != nil {
						unilateralPermFlags.Store(true)
					}
				},
			},
		})
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		aclCmd := c.SetACL("INBOX", imap.RightsIdentifier("user"), imap.RightModificationReplace, imap.RightSet("lr"))
		selCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		if err := aclCmd.Wait(); err != nil {
			t.Fatalf("SetACL: %v", err)
		}
		if _, err := selCmd.Wait(); err == nil {
			t.Fatal("expected BAD from SELECT")
		}
		if !unilateralPermFlags.Load() {
			t.Errorf("UnilateralDataHandler.Mailbox was not called for PERMANENTFLAGS update during SETACL INBOX")
		}
	})

	t.Run("AppendCatenate_relative_URL_from_selected_mailbox_before_BAD_SELECT", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 CATENATE] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* 2 EXISTS\r\n"+
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
		catCmd := c.AppendCatenate("Drafts", nil)
		catCmd.URL(";UID=1")
		_ = catCmd.Close()
		selCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		if _, err := catCmd.Wait(); err != nil {
			t.Fatalf("AppendCatenate: %v", err)
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

	t.Run("List_ReturnStatus_on_selected_mailbox_before_BAD_SELECT", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = fmt.Fprint(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 LIST-EXTENDED LIST-STATUS] ready\r\n")

			line1, _ := br.ReadString('\n')
			tag1, _, _ := strings.Cut(line1, " ")
			_, _ = fmt.Fprintf(sConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag1)

			line2, _ := br.ReadString('\n')
			tag2, _, _ := strings.Cut(line2, " ")
			line3, _ := br.ReadString('\n')
			tag3, _, _ := strings.Cut(line3, " ")
			_, _ = fmt.Fprintf(sConn,
				"* LIST () \"/\" INBOX\r\n"+
					"* STATUS INBOX (MESSAGES 2)\r\n"+
					"* 2 EXISTS\r\n"+
					"%s OK LIST completed\r\n"+
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
		listCmd := c.List("", "INBOX", &imap.ListOptions{
			ReturnStatus: &imap.StatusOptions{NumMessages: true},
		})
		selCmd := c.Select("Archive", &imap.SelectOptions{CondStore: true})

		if _, err := listCmd.Collect(); err != nil {
			t.Fatalf("List: %v", err)
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
}
