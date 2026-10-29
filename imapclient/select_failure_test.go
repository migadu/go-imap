package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 3501 §6.3.1 and RFC 9051 §6.3.2: "if a mailbox is selected and a SELECT
// command that fails is attempted, no mailbox is selected", and NO leaves the
// connection in the authenticated state. EXAMINE is the same. The server here
// never sends [CLOSED], as an IMAP4rev1 server without QRESYNC does not, so
// the client has to take it from the tagged NO.
func TestSelectFailureDeselects(t *testing.T) {
	for _, tc := range []struct {
		name        string
		selectFirst bool // SELECT INBOX successfully first
		examine     bool
		status      string // tagged reply to the refused command
		wantState   imap.ConnState
		wantMailbox string // "" for none
	}{
		{name: "SELECT NO", selectFirst: true, status: "NO no such mailbox", wantState: imap.ConnStateAuthenticated},
		{name: "EXAMINE NO", selectFirst: true, examine: true, status: "NO no such mailbox", wantState: imap.ConnStateAuthenticated},
		// BAD rejects the command as malformed, so the server did not act on
		// it and the earlier mailbox stays selected.
		{name: "SELECT BAD", selectFirst: true, status: "BAD invalid arguments", wantState: imap.ConnStateSelected, wantMailbox: "INBOX"},
		// NO only leaves the selected state; it must not promote a client
		// that never logged in.
		{name: "SELECT NO before login", status: "NO not logged in", wantState: imap.ConnStateNotAuthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}

			var replies []string
			if tc.selectFirst {
				replies = append(replies, "* 3 EXISTS\r\n* OK [UIDVALIDITY 11] ok\r\n%s OK [READ-WRITE] SELECT completed\r\n")
			}
			replies = append(replies, "%s "+tc.status+"\r\n")
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

			if tc.selectFirst {
				if _, err := client.Select("INBOX", nil).Wait(); err != nil {
					t.Fatalf("Select(INBOX) = %v", err)
				}
			}
			_, err := client.Select("Gone", &imap.SelectOptions{ReadOnly: tc.examine}).Wait()
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) {
				t.Fatalf("Select(Gone) = %v, want an IMAP error", err)
			}

			// Wait returns only once the state is final.
			if state := client.State(); state != tc.wantState {
				t.Errorf("State() = %v, want %v", state, tc.wantState)
			}
			mbox := client.Mailbox()
			if tc.wantMailbox == "" && mbox != nil {
				t.Errorf("Mailbox() = %+v, want nil", mbox)
			} else if tc.wantMailbox != "" && (mbox == nil || mbox.Name != tc.wantMailbox) {
				t.Errorf("Mailbox() = %+v, want %q", mbox, tc.wantMailbox)
			}
		})
	}
}
