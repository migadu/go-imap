package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 9051 §7.1: READ-ONLY and READ-WRITE also report that the access to the
// selected mailbox has changed, so Mailbox().ReadOnly must follow them.
func TestMailboxReadOnlyChanges(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	// Answers commands in order; the NOOPs carry the access changes.
	replies := []string{
		"* 1 EXISTS\r\n%s OK [READ-WRITE] SELECT completed\r\n",
		"* OK [READ-ONLY] Permissions changed\r\n%s OK NOOP completed\r\n",
		"* OK [READ-WRITE] Permissions changed\r\n%s OK NOOP completed\r\n",
		"* 1 EXISTS\r\n%s OK [READ-ONLY] EXAMINE completed\r\n",
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
		mbox := client.Mailbox()
		if mbox == nil {
			t.Fatalf("%s: Mailbox() = nil", step)
		}
		if mbox.ReadOnly != want {
			t.Errorf("%s: Mailbox().ReadOnly = %v, want %v", step, mbox.ReadOnly, want)
		}
	}

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}
	check("after SELECT", false)

	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	check("after untagged READ-ONLY", true)

	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop() = %v", err)
	}
	check("after untagged READ-WRITE", false)

	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("Select(ReadOnly) = %v", err)
	}
	check("after EXAMINE", true)
}
