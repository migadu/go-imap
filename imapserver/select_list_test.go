package imapserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// selectListSession populates SelectData.List, which RFC 9051 section 6.3.2
// lists among the required untagged responses of SELECT.
type selectListSession struct {
	imapserver.Session
}

func (s *selectListSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := s.Session.Select(ctx, mailbox, options)
	if err != nil {
		return nil, err
	}
	data.List = &imap.ListData{Mailbox: mailbox, Delim: '/'}
	return data, nil
}

// TestSelectWithListData verifies that a backend may populate SelectData.List:
// the LIST response must be written inside the SELECT response block and the
// command must complete. Writing it used to re-enter the response encoder,
// deadlocking the connection goroutine on the non-reentrant encoder mutex.
func TestSelectWithListData(t *testing.T) {
	user := imapmemserver.NewUser("user", "pass")
	memServer := imapmemserver.New()
	memServer.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &selectListSession{Session: memServer.NewSession()}, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapNotify:    {},
		},
		InsecureAuth: true,
	})

	ln := mustListen(t)
	defer srv.Close()
	go srv.Serve(ln)

	c := dialNotifyTest(t, ln.Addr().String())
	defer c.close()
	c.login(t)
	if resp := c.cmd("CREATE INBOX"); !strings.Contains(resp, "OK") {
		t.Fatalf("CREATE failed: %q", resp)
	}

	resp := c.cmd("SELECT INBOX")
	if !strings.Contains(resp, "OK") {
		t.Fatalf("SELECT did not complete: %q", resp)
	}
	if !strings.Contains(resp, "* LIST") || !strings.Contains(resp, "INBOX") {
		t.Errorf("expected a LIST response in the SELECT response block, got %q", resp)
	}
}
