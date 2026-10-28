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

// A LIST response that no reading of the arguments explains is still taken
// for an unsolicited one, as NOTIFY and RFC 9051 §6.3.4 send.
func TestListUnrelatedResponseStaysUnsolicited(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		line, err := bufio.NewReader(serverConn).ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		fmt.Fprint(serverConn, "* LIST () \"/\" Archive/2024\r\n")
		fmt.Fprint(serverConn, "* LIST () \"/\" INBOX\r\n")
		fmt.Fprintf(serverConn, "%s OK LIST completed\r\n", tag)
	}()

	unsolicited := make(chan string, 1)
	client := imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			List: func(data *imap.ListData) { unsolicited <- data.Mailbox },
		},
	})
	defer client.Close()

	mailboxes, err := client.List("", "INBOX", nil).Collect()
	if err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	if len(mailboxes) != 1 || mailboxes[0].Mailbox != "INBOX" {
		t.Errorf("List(\"\", \"INBOX\") returned %v mailboxes, want only INBOX", len(mailboxes))
	}
	select {
	case name := <-unsolicited:
		if name != "Archive/2024" {
			t.Errorf("unsolicited LIST for %q, want Archive/2024", name)
		}
	default:
		t.Errorf("Archive/2024 was not reported as unsolicited")
	}
}

// RFC 3501 §6.3.8 and RFC 9051 §6.3.9 leave the reading of the reference
// argument to the server; on a UNIX-based one a mailbox argument that names an
// absolute path overrides the reference. The client cannot tell such a reply
// from an unrelated one, so it must not drop it.
func TestListReferenceInterpretedByServer(t *testing.T) {
	for _, tc := range []struct {
		ref, pattern, mailbox string
	}{
		{"~smith/Mail/", "/usr/doc/foo", "/usr/doc/foo"},
		{"archive/", "~fred/Mail/*", "~fred/Mail/foo"},
	} {
		for _, name := range []string{"LIST", "LSUB"} {
			t.Run(name+" "+tc.ref+" "+tc.pattern, func(t *testing.T) {
				clientConn, serverConn := net.Pipe()
				defer serverConn.Close()

				go func() {
					fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
					line, err := bufio.NewReader(serverConn).ReadString('\n')
					if err != nil {
						return
					}
					tag, _, _ := strings.Cut(line, " ")
					fmt.Fprintf(serverConn, "* %s () \"/\" %s\r\n", name, tc.mailbox)
					fmt.Fprintf(serverConn, "%s OK %s completed\r\n", tag, name)
				}()

				client := imapclient.New(clientConn, nil)
				defer client.Close()

				var cmd *imapclient.ListCommand
				if name == "LSUB" {
					cmd = client.Lsub(tc.ref, tc.pattern)
				} else {
					cmd = client.List(tc.ref, tc.pattern, nil)
				}
				mailboxes, err := cmd.Collect()
				if err != nil {
					t.Fatalf("Collect() = %v", err)
				}
				if len(mailboxes) != 1 || mailboxes[0].Mailbox != tc.mailbox {
					var got []string
					for _, mbox := range mailboxes {
						got = append(got, mbox.Mailbox)
					}
					t.Errorf("%s %q %q = %q, want [%q]", name, tc.ref, tc.pattern, got, tc.mailbox)
				}
			})
		}
	}
}
