package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 7162 sections 3.2.10.2 and 3.2.11: live VANISHED updates apply after
// EXISTS even before SELECT completes, and CLOSED separates the old mailbox's
// updates from those for the new mailbox. EARLIER must not lower either count.
func TestVanishedCountDuringSelect(t *testing.T) {
	for _, command := range []string{"SELECT", "EXAMINE"} {
		for _, tc := range []struct {
			name          string
			switchMailbox bool
			sameMailbox   bool
			oldExists     bool
			oldVanished   bool
			updates       string
			wantCount     uint32
			wantLive      int
			wantEarlier   uint64
		}{
			{name: "initial selection", updates: "* VANISHED 2:3\r\n", wantCount: 3, wantLive: 1},
			{name: "switch after CLOSED", switchMailbox: true, updates: "* VANISHED 2:3\r\n", wantCount: 3, wantLive: 1},
			{name: "same mailbox without CLOSED", sameMailbox: true, updates: "* VANISHED 2:3\r\n", wantCount: 3, wantLive: 1},
			{name: "several live responses", updates: "* VANISHED 2\r\n* VANISHED 3:4\r\n", wantCount: 2, wantLive: 2},
			// An invalid server count must saturate instead of wrapping around.
			{name: "clamp inconsistent count", switchMailbox: true, updates: "* VANISHED 2:9\r\n", wantCount: 0, wantLive: 1},
			{name: "initial EARLIER", updates: "* VANISHED (EARLIER) 12:13\r\n", wantCount: 5, wantEarlier: 2},
			{name: "EARLIER after CLOSED", switchMailbox: true, updates: "* VANISHED (EARLIER) 12:13\r\n", wantCount: 5, wantEarlier: 2},
			{name: "old mailbox before CLOSED", switchMailbox: true, oldVanished: true, wantCount: 5},
			{name: "live responses on both sides of CLOSED", switchMailbox: true, oldVanished: true, updates: "* VANISHED 2:3\r\n", wantCount: 3, wantLive: 1},
			{name: "old EXISTS before CLOSED is replaced", switchMailbox: true, oldExists: true, oldVanished: true, updates: "* VANISHED 2:3\r\n", wantCount: 3, wantLive: 1},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				type exchange struct{ command, response string }
				script := []exchange{{"ENABLE QRESYNC", "* ENABLED QRESYNC\r\n%s OK enabled\r\n"}}
				if tc.switchMailbox || tc.sameMailbox {
					uidValidity, uidNext := 10, 106
					if tc.sameMailbox {
						uidValidity, uidNext = 11, 14
					}
					script = append(script, exchange{"SELECT INBOX", fmt.Sprintf("* FLAGS (\\Seen)\r\n* 5 EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY %d] valid\r\n* OK [UIDNEXT %d] next\r\n* OK [PERMANENTFLAGS (\\Seen)] flags\r\n* OK [HIGHESTMODSEQ 1] modseq\r\n%%s OK [READ-WRITE] selected\r\n", uidValidity, uidNext)})
				}
				var response strings.Builder
				if tc.oldExists {
					response.WriteString("* 5 EXISTS\r\n")
				}
				if tc.oldVanished {
					response.WriteString("* VANISHED 101:102\r\n")
				}
				if tc.switchMailbox {
					response.WriteString("* OK [CLOSED] old mailbox closed\r\n")
				}
				response.WriteString("* FLAGS (\\Seen)\r\n* 5 EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 11] valid\r\n* OK [UIDNEXT 14] next\r\n* OK [PERMANENTFLAGS (\\Seen)] flags\r\n* OK [HIGHESTMODSEQ 2] modseq\r\n")
				response.WriteString(tc.updates)
				access := "READ-WRITE"
				if command == "EXAMINE" {
					access = "READ-ONLY"
				}
				fmt.Fprintf(&response, "%%s OK [%s] selected\r\n", access)
				mailboxName, mailboxArg := "Other", "\"Other\""
				if tc.sameMailbox {
					mailboxName, mailboxArg = "INBOX", "INBOX"
				}
				script = append(script, exchange{command + " " + mailboxArg + " (QRESYNC (11 1))", response.String()})

				clientConn, serverConn := net.Pipe()
				if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				serverErr := make(chan error, 1)
				serverDone := make(chan struct{})
				go func() {
					defer close(serverDone)
					err := func() error {
						if _, err := io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ENABLE CONDSTORE QRESYNC] ready\r\n"); err != nil {
							return fmt.Errorf("greeting: %w", err)
						}
						r := bufio.NewReader(serverConn)
						for _, step := range script {
							line, err := r.ReadString('\n')
							if err != nil {
								return fmt.Errorf("read %s: %w", step.command, err)
							}
							tag, got, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
							if !ok || tag == "" || got != step.command {
								return fmt.Errorf("got command %q, want tagged %q", line, step.command)
							}
							if _, err := io.WriteString(serverConn, strings.ReplaceAll(step.response, "%s", tag)); err != nil {
								return fmt.Errorf("reply to %s: %w", step.command, err)
							}
						}
						return nil
					}()
					if err != nil {
						serverConn.Close()
					}
					serverErr <- err
				}()

				type observation struct {
					uids, mailbox string
					count         uint32
					earlier       bool
				}
				var client *imapclient.Client
				var seen []observation
				client = imapclient.New(clientConn, &imapclient.Options{
					ResponseTimeout: 2 * time.Second,
					UnilateralDataHandler: &imapclient.UnilateralDataHandler{Vanished: func(data *imap.VanishedData) {
						obs := observation{uids: data.UIDs.String(), earlier: data.Earlier}
						if mailbox := client.Mailbox(); mailbox != nil {
							obs.mailbox = mailbox.Name
							obs.count = mailbox.NumMessages
						}
						seen = append(seen, obs)
					}},
				})
				t.Cleanup(func() {
					client.Close()
					serverConn.Close()
					if err := <-serverErr; err != nil {
						t.Errorf("server: %v", err)
					}
				})
				if _, err := client.Enable(imap.CapQResync).Wait(); err != nil {
					t.Fatalf("Enable: %v", err)
				}
				var oldMailbox *imapclient.SelectedMailbox
				if tc.switchMailbox || tc.sameMailbox {
					if _, err := client.Select("INBOX", nil).Wait(); err != nil {
						t.Fatalf("initial Select: %v", err)
					}
					oldMailbox = client.Mailbox()
				}
				data, err := client.Select(mailboxName, &imap.SelectOptions{ReadOnly: command == "EXAMINE", QResync: &imap.QResyncData{UIDValidity: 11, ModSeq: 1}}).Wait()
				if err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				<-serverDone
				if data.NumMessages != tc.wantCount {
					t.Errorf("SelectData.NumMessages = %d, want %d", data.NumMessages, tc.wantCount)
				}
				mailbox := client.Mailbox()
				if mailbox == nil || mailbox.Name != mailboxName || mailbox.NumMessages != tc.wantCount {
					t.Errorf("Mailbox() = %+v, want %s with %d messages", mailbox, mailboxName, tc.wantCount)
				}
				if data.Vanished.Cardinality() != tc.wantEarlier {
					t.Errorf("SelectData.Vanished = %v, want cardinality %d", data.Vanished, tc.wantEarlier)
				}
				if oldMailbox != nil && oldMailbox.NumMessages != 5 {
					t.Errorf("old mailbox snapshot changed: %+v", oldMailbox)
				}
				wantCallbacks := tc.wantLive
				if tc.oldVanished {
					wantCallbacks++
				}
				if len(seen) != wantCallbacks {
					t.Fatalf("VANISHED callbacks = %+v, want %d", seen, wantCallbacks)
				}
				if tc.oldVanished && (seen[0].uids != "101:102" || seen[0].mailbox != "INBOX" || seen[0].count != 3) {
					t.Errorf("old mailbox callback = %+v, want INBOX with 3 messages", seen[0])
				}
				if tc.sameMailbox && (seen[0].uids != "2:3" || seen[0].mailbox != "INBOX" || seen[0].count != 3) {
					t.Errorf("reselected mailbox callback = %+v, want INBOX with 3 messages", seen[0])
				}
				for _, obs := range seen {
					if obs.earlier {
						t.Errorf("EARLIER routed to unilateral callback: %+v", obs)
					}
				}
			})
		}
	}
}
