package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 7162 section 7 permits only the EARLIER modifier. An unrecognized
// parenthesized token must not become a live removal and change the selected
// mailbox's message count. A UID FETCH permits both valid VANISHED forms.
func TestVanishedModifierRequiresASCIIEarlier(t *testing.T) {
	for _, tc := range []struct {
		name, modifier string
		valid, earlier bool
		count          uint32
	}{
		{name: "uppercase historical", modifier: "EARLIER", valid: true, earlier: true, count: 5},
		{name: "mixed ASCII historical", modifier: "eArLiEr", valid: true, earlier: true, count: 5},
		{name: "absent live", valid: true, count: 3},
		{name: "Unicode dotless I", modifier: "EARLıER"},
		{name: "Unicode dotted I", modifier: "EARLİER"},
		{name: "unknown ASCII", modifier: "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, srv := net.Pipe()
			if err := srv.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				defer srv.Close()
				if _, err := io.WriteString(srv, "* PREAUTH [CAPABILITY IMAP4rev2 QRESYNC CONDSTORE] ready\r\n"); err != nil {
					done <- err
					return
				}
				br := bufio.NewReader(srv)
				for _, command := range []string{"ENABLE QRESYNC", "SELECT ", "UID FETCH "} {
					line, err := br.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					tag, body, ok := strings.Cut(line, " ")
					if !ok || !strings.HasPrefix(body, command) {
						done <- fmt.Errorf("command = %q, want %q", line, command)
						return
					}
					var data string
					switch command {
					case "ENABLE QRESYNC":
						data = "* ENABLED QRESYNC CONDSTORE\r\n"
					case "SELECT ":
						data = "* 5 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [HIGHESTMODSEQ 1] unchanged\r\n"
					default:
						modifier := ""
						if tc.modifier != "" {
							modifier = "(" + tc.modifier + ") "
						}
						data = "* VANISHED " + modifier + "2:3\r\n"
					}
					if _, err := fmt.Fprintf(srv, "%s%s OK completed\r\n", data, tag); err != nil {
						done <- err
						return
					}
				}
				_, err := io.Copy(io.Discard, br)
				done <- err
			}()
			type event struct {
				earlier bool
				count   uint32
			}
			events := make(chan event, 1)
			var c *imapclient.Client
			c = imapclient.New(cli, &imapclient.Options{
				ResponseTimeout: 2 * time.Second,
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Vanished: func(data *imap.VanishedData) {
						events <- event{earlier: data.Earlier, count: c.Mailbox().NumMessages}
					},
				},
			})
			t.Cleanup(func() { c.Close(); srv.Close() })
			if _, err := c.Enable(imap.CapQResync).Wait(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			_, err := c.Fetch(imap.UIDSetNum(1, 2, 3, 4, 5), &imap.FetchOptions{
				Flags: true, ChangedSince: 1, Vanished: true,
			}).Collect()
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-events:
					if got.earlier != tc.earlier || got.count != tc.count {
						t.Errorf("VANISHED event = %+v, want Earlier=%v count=%d", got, tc.earlier, tc.count)
					}
				default:
					t.Error("valid VANISHED response was not delivered")
				}
			} else {
				if err == nil {
					t.Error("unrecognized VANISHED modifier was accepted")
				}
				select {
				case got := <-events:
					t.Errorf("invalid modifier reached VANISHED handler: Earlier=%v selected count=%d", got.earlier, got.count)
				default:
				}
			}
			c.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
