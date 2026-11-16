package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// A selected mailbox is present even when its name is empty. Keep an update
// emitted by GETACL "" out of a later SELECT, without waiting for that SELECT
// to finish before updating the old mailbox or calling its handler.
func TestEmptySelectedMailboxCommandBarrier(t *testing.T) {
	releaseSelect := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSelect) }) }
	var exists atomic.Uint32
	c, done := newSelectIsolation307Peer(t, "IMAP4rev1 ACL", &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				exists.Store(*data.NumMessages)
			}
		}},
	}, func(br *bufio.Reader, conn net.Conn) error {
		tag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if err := writeSelectIsolation307Data(conn, tag, 1, `\Answered`, `\Answered`); err != nil {
			return err
		}
		previous, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		selection, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "* 2 EXISTS\r\n* ACL \"\" user lr\r\n%s OK earlier command\r\n", previous); err != nil {
			return err
		}
		<-releaseSelect
		if _, err := fmt.Fprintf(conn, "%s BAD invalid SELECT\r\n", selection); err != nil {
			return err
		}
		return finishSelectIsolation307Noop(br, conn)
	})
	t.Cleanup(release)
	if _, err := c.Select("", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	acl := c.GetACL("")
	selection := c.Select("Other", &imap.SelectOptions{CondStore: true})
	if _, err := acl.Wait(); err != nil {
		t.Fatal(err)
	}
	// The server is still withholding the later SELECT's completion. Capture
	// what GETACL made visible now, before BAD can replay provisional updates.
	oldMailbox, callback := c.Mailbox(), exists.Load()
	release()
	if _, err := selection.Wait(); err == nil {
		t.Fatal("expected SELECT BAD")
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if oldMailbox == nil || oldMailbox.Name != "" || oldMailbox.NumMessages != 2 || callback != 2 {
		t.Fatalf("after GETACL completed: mailbox=%+v, EXISTS callback=%d; want empty-named mailbox and callback at 2 before later SELECT finishes", oldMailbox, callback)
	}
}

// This is a real mailbox name supported by the in-memory server, rather than
// an absent selection represented by a synthetic client state.
func TestEmptyMailboxCanBeSelected(t *testing.T) {
	c, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer server.Close()
	defer c.Close()
	if err := c.Create("", nil).Wait(); err != nil {
		t.Fatalf("CREATE empty mailbox: %v", err)
	}
	if _, err := c.Select("", nil).Wait(); err != nil {
		t.Fatalf("SELECT empty mailbox: %v", err)
	}
	if mailbox := c.Mailbox(); mailbox == nil || mailbox.Name != "" {
		t.Fatalf("selected mailbox = %+v, want empty-named mailbox", mailbox)
	}
}
