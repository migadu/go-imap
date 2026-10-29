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

// A LIST response that no reading of the arguments explains is still taken
// for an unsolicited one, as NOTIFY and RFC 9051 §6.3.4 send.
func TestListUnrelatedResponseStaysUnsolicited(t *testing.T) {
	for _, tc := range []struct {
		ref, pattern, wanted, unrelated string
		nilDelim                        bool
	}{
		{ref: "", pattern: "INBOX", wanted: "INBOX", unrelated: "Archive/2024"},
		{ref: "Archive/", pattern: "*", wanted: "Archive/2024", unrelated: "INBOX"},
		{ref: "Archive/", pattern: "%", wanted: "Archive/2024", unrelated: "INBOX"},
		{ref: "fo*o/bar", pattern: "rag/baz", wanted: "fo*o/rag/baz", unrelated: "fooo/rag/baz"},
		// Without a hierarchy delimiter a reference has no parent to resolve
		// against, so a wildcard-leading pattern stays under the reference
		// whatever the reference's form.
		{ref: "Archive/", pattern: "*", wanted: "Archive/2024", unrelated: "INBOX", nilDelim: true},
		{ref: "Archive", pattern: "*", wanted: "Archive/2024", unrelated: "INBOX", nilDelim: true},
	} {
		name := tc.ref + " " + tc.pattern
		delim := `"/"`
		if tc.nilDelim {
			name += " NIL delimiter"
			delim = "NIL"
		}
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))

			go func() {
				fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
				line, err := bufio.NewReader(serverConn).ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				fmt.Fprintf(serverConn, "* LIST () %s %q\r\n", delim, tc.unrelated)
				fmt.Fprintf(serverConn, "* LIST () %s %q\r\n", delim, tc.wanted)
				fmt.Fprintf(serverConn, "%s OK LIST completed\r\n", tag)
			}()

			unsolicited := make(chan string, 2)
			client := imapclient.New(clientConn, &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					List: func(data *imap.ListData) { unsolicited <- data.Mailbox },
				},
			})
			defer client.Close()

			mailboxes, err := client.List(tc.ref, tc.pattern, nil).Collect()
			if err != nil {
				t.Fatalf("Collect() = %v", err)
			}
			if len(mailboxes) != 1 || mailboxes[0].Mailbox != tc.wanted {
				t.Errorf("List(%q, %q) = %q, want [%q]", tc.ref, tc.pattern, mailboxNames(mailboxes), tc.wanted)
			}
			select {
			case name := <-unsolicited:
				if name != tc.unrelated {
					t.Errorf("unsolicited LIST for %q, want %q", name, tc.unrelated)
				}
			default:
				t.Errorf("%q was not reported as unsolicited", tc.unrelated)
			}
			select {
			case name := <-unsolicited:
				t.Errorf("unexpected unsolicited LIST for %q", name)
			default:
			}
		})
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
		{"archive/", "#news.comp.mail.*", "#news.comp.mail.imap"},
		// Breakout characters are server-defined, not limited to /, ~ or #.
		{"archive/", "$shared/*", "$shared/2024"},
		// All three implementation-dependent interpretations in RFC 9051.
		{"foo/bar", "rag/baz", "foo/bar/rag/baz"},
		{"foo/bar", "rag/baz", "foo/barrag/baz"},
		{"foo/bar", "rag/baz", "foo/rag/baz"},
		// The same interpretations can contain wildcard patterns.
		{"foo/bar", "rag/*", "foo/barrag/baz"},
		{"foo/bar", "rag/%", "foo/rag/baz"},
		// A reference without a delimiter can be resolved relative to its
		// empty parent. These valid replies need the pattern-only fallback,
		// even though the same names could also be unsolicited notifications.
		{"Archive", "*", "INBOX"},
		{"Archive", "%", "INBOX"},
	} {
		for _, name := range []string{"LIST", "LSUB"} {
			t.Run(name+" "+tc.ref+" "+tc.pattern, func(t *testing.T) {
				clientConn, serverConn := net.Pipe()
				defer serverConn.Close()
				serverConn.SetDeadline(time.Now().Add(5 * time.Second))
				clientConn.SetDeadline(time.Now().Add(5 * time.Second))

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

// LSUB has no unsolicited responses, so an unmatched reply still belongs to
// the pending LSUB command even when a LIST would send it to the handler.
func TestLsubReferenceFallback(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		line, err := bufio.NewReader(serverConn).ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		fmt.Fprint(serverConn, "* LSUB () \"/\" INBOX\r\n")
		fmt.Fprintf(serverConn, "%s OK LSUB completed\r\n", tag)
	}()

	client := imapclient.New(clientConn, nil)
	defer client.Close()
	mailboxes, err := client.Lsub("Archive/", "*").Collect()
	if err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	if len(mailboxes) != 1 || mailboxes[0].Mailbox != "INBOX" {
		t.Errorf("Lsub() = %q, want [INBOX]", mailboxNames(mailboxes))
	}
}
