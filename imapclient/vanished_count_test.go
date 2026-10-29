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

// RFC 7162 §3.2.10.2: "Unlike VANISHED (EARLIER), this response also
// decrements the number of messages in the mailbox", one per UID, and the
// server need not send EXISTS for the new count. VANISHED (EARLIER) reports
// messages expunged before and leaves the count alone (§3.2.10.1).
func TestVanishedLowersMessageCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exists   uint32
		vanished string // the server's answer to NOOP
		want     uint32
	}{
		{name: "one UID", exists: 5, vanished: "* VANISHED 2", want: 4},
		{name: "a set", exists: 5, vanished: "* VANISHED 2,4:5", want: 2},
		{name: "several responses", exists: 5, vanished: "* VANISHED 1\r\n* VANISHED 3:4", want: 2},
		{name: "more than the count", exists: 1, vanished: "* VANISHED 3:4", want: 0},
		{name: "EARLIER", exists: 5, vanished: "* VANISHED (EARLIER) 2:3", want: 5},
		{name: "dynamic set", exists: 5, vanished: "* VANISHED 4:*", want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}

			replies := []string{
				"* ENABLED QRESYNC\r\n%s OK ENABLE completed\r\n",
				fmt.Sprintf("* %d EXISTS\r\n* OK [UIDVALIDITY 11] ok\r\n%%s OK [READ-WRITE] SELECT completed\r\n", tc.exists),
				tc.vanished + "\r\n%s OK NOOP completed\r\n",
			}
			go func() {
				fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 ENABLE CONDSTORE QRESYNC] ready\r\n")
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

			// The handler sees the count already lowered, as an Expunge
			// handler does.
			var client *imapclient.Client
			var seen []uint32
			client = imapclient.New(clientConn, &imapclient.Options{
				ResponseTimeout: 2 * time.Second,
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Vanished: func(*imap.VanishedData) {
						seen = append(seen, client.Mailbox().NumMessages)
					},
				},
			})
			defer client.Close()

			if _, err := client.Enable(imap.CapQResync).Wait(); err != nil {
				t.Fatalf("Enable(QRESYNC) = %v", err)
			}
			if _, err := client.Select("INBOX", nil).Wait(); err != nil {
				t.Fatalf("Select(INBOX) = %v", err)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop() = %v", err)
			}

			if got := client.Mailbox().NumMessages; got != tc.want {
				t.Errorf("Mailbox().NumMessages = %d, want %d", got, tc.want)
			}
			if len(seen) == 0 || seen[len(seen)-1] != tc.want {
				t.Errorf("the Vanished handler saw counts %v, want the last to be %d", seen, tc.want)
			}
		})
	}
}

// VANISHED (EARLIER) answering a QRESYNC SELECT goes to SelectData and leaves
// the count the SELECT's EXISTS gave.
func TestVanishedEarlierDuringSelectKeepsTheCount(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 ENABLE CONDSTORE QRESYNC] ready\r\n")
		r := bufio.NewReader(serverConn)
		for _, reply := range []string{
			"* ENABLED QRESYNC\r\n%s OK ENABLE completed\r\n",
			"* VANISHED (EARLIER) 1:3\r\n* 4 EXISTS\r\n* OK [UIDVALIDITY 11] ok\r\n%s OK [READ-WRITE] SELECT completed\r\n",
		} {
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
	if _, err := client.Enable(imap.CapQResync).Wait(); err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}
	data, err := client.Select("INBOX", &imap.SelectOptions{
		QResync: &imap.QResyncData{UIDValidity: 11, ModSeq: 1},
	}).Wait()
	if err != nil {
		t.Fatalf("Select(INBOX) = %v", err)
	}
	if data.NumMessages != 4 || client.Mailbox().NumMessages != 4 {
		t.Errorf("SelectData.NumMessages = %d, Mailbox().NumMessages = %d, want 4 and 4", data.NumMessages, client.Mailbox().NumMessages)
	}
	if data.Vanished.Cardinality() != 3 {
		t.Errorf("SelectData.Vanished = %v, want 1:3", data.Vanished)
	}
}
