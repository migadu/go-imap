package imapclient_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// notifyLiteral adapts a byte slice to imap.LiteralReader for direct
// server-side appends (simulating external delivery, e.g. by an MDA).
type notifyLiteral struct {
	*strings.Reader
}

func (l notifyLiteral) Size() int64 {
	return int64(l.Reader.Size())
}

func newNotifyLiteral(s string) notifyLiteral {
	return notifyLiteral{strings.NewReader(s)}
}

const notifyTestMessage = "From: sender@example.com\r\n" +
	"To: recipient@example.com\r\n" +
	"Subject: NOTIFY test\r\n" +
	"\r\n" +
	"body\r\n"

// newNotifyIntegrationServer starts an in-memory server with NOTIFY enabled
// and returns its address plus the backing user, so tests can trigger
// server-side events directly.
func newNotifyIntegrationServer(t *testing.T, mailboxes ...string) (addr string, user *imapmemserver.User, closer func()) {
	t.Helper()

	user = imapmemserver.NewUser(testUsername, testPassword)
	for _, name := range mailboxes {
		if err := user.Create(context.Background(), name, nil); err != nil {
			t.Fatalf("Create(%q) = %v", name, err)
		}
	}

	memServer := imapmemserver.New()
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapCondStore: {},
			imap.CapNotify:    {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Errorf("Serve() = %v", err)
		}
	}()

	return ln.Addr().String(), user, func() { server.Close() }
}

func dialNotifyClient(t *testing.T, addr string, options *imapclient.Options) *imapclient.Client {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	client := imapclient.New(conn, options)
	if err := client.Login(testUsername, testPassword).Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	return client
}

// TestNotifyIntegration_StatusOnExternalAppend verifies that a message
// delivered to a non-selected mailbox is reported with an unsolicited STATUS
// response (RFC 5465 section 5.2), and that message events for the selected
// mailbox stay queued until the next command when no SELECTED specifier is
// active (RFC 5465 section 3.1).
func TestNotifyIntegration_StatusOnExternalAppend(t *testing.T) {
	addr, user, closer := newNotifyIntegrationServer(t, "INBOX", "Archive")
	defer closer()

	statusCh := make(chan *imap.StatusData, 8)
	existsCh := make(chan uint32, 8)
	client := dialNotifyClient(t, addr, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Status: func(data *imap.StatusData) {
				statusCh <- data
			},
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					existsCh <- *data.NumMessages
				}
			},
		},
	})
	defer client.Close()

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}

	cmd, err := client.Notify(&imap.NotifyOptions{
		Items: []imap.NotifyItem{
			{
				MailboxSpec: imap.NotifyMailboxSpecPersonal,
				Events: []imap.NotifyEvent{
					imap.NotifyEventMessageNew,
					imap.NotifyEventMessageExpunge,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Notify() = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Notify().Wait() = %v", err)
	}

	// External delivery to the non-selected mailbox: unsolicited STATUS.
	if _, err := user.Append(context.Background(), "Archive", newNotifyLiteral(notifyTestMessage), &imap.AppendOptions{}); err != nil {
		t.Fatalf("Append(Archive) = %v", err)
	}
	select {
	case data := <-statusCh:
		if data.Mailbox != "Archive" {
			t.Errorf("STATUS for mailbox %q, want Archive", data.Mailbox)
		}
		if data.NumMessages == nil || *data.NumMessages != 1 {
			t.Errorf("STATUS NumMessages = %v, want 1", data.NumMessages)
		}
		if data.UIDNext != 2 {
			t.Errorf("STATUS UIDNext = %v, want 2", data.UIDNext)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for unsolicited STATUS after external append")
	}

	// External delivery to the selected mailbox: no SELECTED specifier is
	// active, so nothing may arrive until the client issues a command.
	if _, err := user.Append(context.Background(), "INBOX", newNotifyLiteral(notifyTestMessage), &imap.AppendOptions{}); err != nil {
		t.Fatalf("Append(INBOX) = %v", err)
	}
	select {
	case n := <-existsCh:
		t.Fatalf("unexpected EXISTS %v for the selected mailbox without a SELECTED specifier", n)
	case data := <-statusCh:
		t.Fatalf("unexpected STATUS for %q: the selected mailbox must not be matched by PERSONAL", data.Mailbox)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing
	}

	// The queued EXISTS is flushed by the next command (implicit NOOP sync).
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	select {
	case n := <-existsCh:
		if n != 1 {
			t.Errorf("EXISTS = %v, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for EXISTS after NOOP")
	}
}

// TestNotifyIntegration_MessageNewFetchAtts verifies that MessageNew fetch
// attributes produce an unsolicited FETCH response following EXISTS for new
// messages in the selected mailbox (RFC 5465 section 5.2).
func TestNotifyIntegration_MessageNewFetchAtts(t *testing.T) {
	addr, user, closer := newNotifyIntegrationServer(t, "INBOX")
	defer closer()

	existsCh := make(chan uint32, 8)
	fetchCh := make(chan *imapclient.FetchMessageBuffer, 8)
	client := dialNotifyClient(t, addr, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					existsCh <- *data.NumMessages
				}
			},
			Fetch: func(msg *imapclient.FetchMessageData) {
				buf, err := msg.Collect()
				if err != nil {
					t.Errorf("Collect() = %v", err)
					return
				}
				fetchCh <- buf
			},
		},
	})
	defer client.Close()

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}

	cmd, err := client.Notify(&imap.NotifyOptions{
		Items: []imap.NotifyItem{
			{
				MailboxSpec: imap.NotifyMailboxSpecSelected,
				Events: []imap.NotifyEvent{
					imap.NotifyEventMessageNew,
					imap.NotifyEventMessageExpunge,
				},
				MessageNewFetch: &imap.FetchOptions{
					UID:   true,
					Flags: true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Notify() = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Notify().Wait() = %v", err)
	}

	if _, err := user.Append(context.Background(), "INBOX", newNotifyLiteral(notifyTestMessage), &imap.AppendOptions{}); err != nil {
		t.Fatalf("Append(INBOX) = %v", err)
	}

	select {
	case n := <-existsCh:
		if n != 1 {
			t.Errorf("EXISTS = %v, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for unsolicited EXISTS")
	}
	select {
	case buf := <-fetchCh:
		if buf.UID != 1 {
			t.Errorf("unsolicited FETCH UID = %v, want 1", buf.UID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for unsolicited FETCH with MessageNew attributes")
	}
}

// TestNotifyIntegration_SelectedDelayedExpunge verifies that SELECTED-DELAYED
// defers EXPUNGE delivery to the next command sync point (RFC 5465 section
// 6.1.2).
func TestNotifyIntegration_SelectedDelayedExpunge(t *testing.T) {
	addr, user, closer := newNotifyIntegrationServer(t, "INBOX")
	defer closer()

	for i := 0; i < 2; i++ {
		if _, err := user.Append(context.Background(), "INBOX", newNotifyLiteral(notifyTestMessage), &imap.AppendOptions{}); err != nil {
			t.Fatalf("Append(INBOX) = %v", err)
		}
	}

	expungeCh := make(chan uint32, 8)
	watcher := dialNotifyClient(t, addr, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Expunge: func(seqNum uint32) {
				expungeCh <- seqNum
			},
		},
	})
	defer watcher.Close()

	if _, err := watcher.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}

	cmd, err := watcher.Notify(&imap.NotifyOptions{
		Items: []imap.NotifyItem{
			{
				MailboxSpec: imap.NotifyMailboxSpecSelectedDelayed,
				Events: []imap.NotifyEvent{
					imap.NotifyEventMessageNew,
					imap.NotifyEventMessageExpunge,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Notify() = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Notify().Wait() = %v", err)
	}

	// A second connection expunges the first message.
	other := dialNotifyClient(t, addr, nil)
	defer other.Close()
	if _, err := other.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}
	storeCmd := other.Store(imap.SeqSetNum(1), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil)
	if err := storeCmd.Close(); err != nil {
		t.Fatalf("Store() = %v", err)
	}
	if err := other.Expunge().Close(); err != nil {
		t.Fatalf("Expunge() = %v", err)
	}

	// SELECTED-DELAYED: the expunge must not be delivered asynchronously.
	select {
	case seqNum := <-expungeCh:
		t.Fatalf("unexpected asynchronous EXPUNGE %v with SELECTED-DELAYED", seqNum)
	case <-time.After(400 * time.Millisecond):
		// expected: nothing
	}

	// The next command flushes it.
	if err := watcher.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	select {
	case seqNum := <-expungeCh:
		if seqNum != 1 {
			t.Errorf("EXPUNGE seqnum = %v, want 1", seqNum)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for EXPUNGE after NOOP")
	}
}

// TestNotifyIntegration_BadEvent verifies that an event unsupported by the
// backend yields a tagged NO with the BADEVENT response code (RFC 5465
// section 4).
func TestNotifyIntegration_BadEvent(t *testing.T) {
	addr, _, closer := newNotifyIntegrationServer(t, "INBOX")
	defer closer()

	client := dialNotifyClient(t, addr, nil)
	defer client.Close()

	cmd, err := client.Notify(&imap.NotifyOptions{
		Items: []imap.NotifyItem{
			{
				MailboxSpec: imap.NotifyMailboxSpecSelected,
				Events: []imap.NotifyEvent{
					imap.NotifyEventMessageNew,
					imap.NotifyEventMessageExpunge,
					imap.NotifyEventAnnotationChange,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Notify() = %v", err)
	}
	err = cmd.Wait()
	if err == nil {
		t.Fatal("Notify().Wait() succeeded, want NO [BADEVENT ...]")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("Notify().Wait() = %v, want *imap.Error", err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo {
		t.Errorf("status response type = %v, want NO", imapErr.Type)
	}
	if imapErr.Code != imap.ResponseCodeBadEvent {
		t.Errorf("response code = %v, want BADEVENT", imapErr.Code)
	}
}
