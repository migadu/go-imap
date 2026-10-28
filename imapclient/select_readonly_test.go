package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 9051 §7.1: READ-ONLY and READ-WRITE also report that the access to the
// selected mailbox has changed, so Mailbox().ReadOnly must follow them.
func TestMailboxReadOnlyChanges(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		name := "untagged"
		if tagged {
			name = "tagged"
		}
		t.Run(name, func(t *testing.T) { testMailboxReadOnlyChanges(t, tagged) })
	}
}

func testMailboxReadOnlyChanges(t *testing.T, tagged bool) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	// Answers commands in order; the NOOPs carry the access changes.
	replies := []string{
		"* 1 EXISTS\r\n%s OK [READ-WRITE] SELECT completed\r\n",
		"* OK [READ-ONLY] Permissions changed\r\n%s OK NOOP completed\r\n",
		"* OK [READ-WRITE] Permissions changed\r\n%s OK NOOP completed\r\n",
		"* 1 EXISTS\r\n%s OK [READ-ONLY] EXAMINE completed\r\n",
	}
	if tagged {
		replies[1] = "%s OK [READ-ONLY] NOOP completed\r\n"
		replies[2] = "%s OK [READ-WRITE] NOOP completed\r\n"
	}
	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		r := bufio.NewReader(serverConn)
		for _, reply := range replies {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			fmt.Fprint(serverConn, strings.ReplaceAll(reply, "%s", tag))
		}
	}()

	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 2 * time.Second})
	defer client.Close()

	var snapshots []struct {
		mailbox  *imapclient.SelectedMailbox
		readOnly bool
	}
	check := func(step string, want bool) {
		t.Helper()
		if state := client.State(); state != imap.ConnStateSelected {
			t.Errorf("%s: State() = %v, want selected", step, state)
		}
		mbox := client.Mailbox()
		if mbox == nil {
			t.Fatalf("%s: Mailbox() = nil", step)
		}
		if mbox.ReadOnly != want {
			t.Errorf("%s: Mailbox().ReadOnly = %v, want %v", step, mbox.ReadOnly, want)
		}
		if mbox.Name != "INBOX" || mbox.NumMessages != 1 {
			t.Errorf("%s: unrelated mailbox metadata changed: %+v", step, mbox)
		}
		for _, snapshot := range snapshots {
			if snapshot.mailbox.ReadOnly != snapshot.readOnly {
				t.Errorf("%s: previous Mailbox() snapshot mutated from %v to %v", step, snapshot.readOnly, snapshot.mailbox.ReadOnly)
			}
		}
		snapshots = append(snapshots, struct {
			mailbox  *imapclient.SelectedMailbox
			readOnly bool
		}{mbox, mbox.ReadOnly})
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}
	check("after SELECT", false)

	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	check("after READ-ONLY", true)

	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	check("after READ-WRITE", false)

	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("Select(ReadOnly) = %v", err)
	}
	check("after EXAMINE", true)
}

// RFC 9051 §7.1: READ-ONLY and READ-WRITE describe the selected mailbox. A
// refusal of COPY into some other, read-only mailbox says nothing about it; a
// refusal of STORE does, and so does a tagged OK.
func TestTaggedReadOnlyScope(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	replies := []string{
		"* 1 EXISTS\r\n%s OK [READ-WRITE] SELECT completed\r\n",
		"%s NO [READ-ONLY] Target mailbox is read-only\r\n",
		"%s NO [READ-ONLY] Mailbox is read-only\r\n",
		"%s OK [READ-WRITE] NOOP completed\r\n",
	}
	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		r := bufio.NewReader(serverConn)
		for _, reply := range replies {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			fmt.Fprint(serverConn, strings.ReplaceAll(reply, "%s", tag))
		}
	}()

	client := imapclient.New(clientConn, nil)
	defer client.Close()

	check := func(step string, want bool) {
		t.Helper()
		if got := client.Mailbox().ReadOnly; got != want {
			t.Errorf("%s: Mailbox().ReadOnly = %v, want %v", step, got, want)
		}
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}
	check("after SELECT", false)

	if _, err := client.Copy(imap.SeqSetNum(1), "Shared/ro").Wait(); err == nil {
		t.Fatalf("Copy() succeeded, want NO")
	}
	check("after COPY refused with READ-ONLY", false)

	store := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil)
	if err := store.Close(); err == nil {
		t.Fatalf("Store() succeeded, want NO")
	}
	check("after STORE refused with READ-ONLY", true)

	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	check("after NOOP OK [READ-WRITE]", false)
}
