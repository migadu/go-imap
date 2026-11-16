package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func selectIsolation307Wait[T any](wait func() (T, error)) func() error {
	return func() error { _, err := wait(); return err }
}

func TestSelectedMailboxCommandBarriersPreserveUpdates(t *testing.T) {
	type testCase struct {
		name, selected, delimiter string
		start                     func(*imapclient.Client) func() error
		reply                     string
	}
	search := func(source *imap.SearchSource, mutate bool) func(*imapclient.Client) func() error {
		return func(c *imapclient.Client) func() error {
			cmd := c.MultiSearch(source, &imap.SearchCriteria{}, nil)
			if mutate {
				// The command has already encoded its source. Later caller
				// reuse of the input must not change response attribution.
				source.Mailboxes[0] = "Unrelated"
			}
			return selectIsolation307Wait(cmd.Wait)
		}
	}
	cases := []testCase{
		{name: "MULTISEARCH explicit mailbox", start: search(&imap.SearchSource{Mailboxes: []string{"inbox"}}, false)},
		{name: "MULTISEARCH source snapshot", start: search(&imap.SearchSource{Mailboxes: []string{"inbox"}}, true)},
		{name: "MULTISEARCH INBOXES", start: search(&imap.SearchSource{Inboxes: true}, false)},
		{name: "MULTISEARCH PERSONAL", start: search(&imap.SearchSource{Personal: true}, false)},
		{name: "MULTISEARCH SUBSCRIBED", start: search(&imap.SearchSource{Subscribed: true}, false)},
		{name: "MULTISEARCH SUBTREE", selected: "Work^2026", delimiter: "^", start: search(&imap.SearchSource{Subtree: []string{"Work"}}, false)},
		{name: "MULTISEARCH SUBTREE-ONE", selected: "Work^2026", delimiter: "^", start: search(&imap.SearchSource{SubtreeOne: []string{"Work"}}, false)},
		{name: "MULTISEARCH selected control", start: search(nil, false)},
		{name: "RENAME ancestor custom delimiter", selected: "Work^2026", delimiter: "^", start: func(c *imapclient.Client) func() error { return c.Rename("Work", "Archive", nil).Wait }},
		{name: "RENAME selected control", start: func(c *imapclient.Client) func() error { return c.Rename("INBOX", "Archive", nil).Wait }},
		{name: "CATENATE relative source", start: func(c *imapclient.Client) func() error {
			cmd := c.AppendCatenate("Archive", nil)
			cmd.URL(";UID=1")
			if err := cmd.Close(); err != nil {
				return func() error { return err }
			}
			return selectIsolation307Wait(cmd.Wait)
		}},
		{name: "LIST RETURN STATUS", start: func(c *imapclient.Client) func() error {
			return selectIsolation307Wait(c.List("", "*", &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true}}).Collect)
		}, reply: "* LIST () \"/\" INBOX\r\n* STATUS INBOX (MESSAGES 2)\r\n"},
		{name: "MYRIGHTS", start: func(c *imapclient.Client) func() error { return selectIsolation307Wait(c.MyRights("inbox").Wait) }, reply: "* MYRIGHTS INBOX lrswipkxtea\r\n"},
		{name: "GETACL", start: func(c *imapclient.Client) func() error { return selectIsolation307Wait(c.GetACL("inbox").Wait) }, reply: "* ACL INBOX user lrswipkxtea\r\n"},
		{name: "SETACL", start: func(c *imapclient.Client) func() error {
			return c.SetACL("inbox", "user", imap.RightModificationReplace, imap.RightSet{imap.RightRead}).Wait
		}},
		{name: "DELETEACL", start: func(c *imapclient.Client) func() error { return c.DeleteACL("inbox", "user").Wait }},
		{name: "LISTRIGHTS", start: func(c *imapclient.Client) func() error {
			return selectIsolation307Wait(c.ListRights("inbox", "user").Wait)
		}, reply: "* LISTRIGHTS INBOX user lrswipkxtea\r\n"},
		{name: "GETMETADATA", start: func(c *imapclient.Client) func() error {
			return selectIsolation307Wait(c.GetMetadata("INBOX", []string{"/shared/comment"}, nil).Wait)
		}, reply: "* METADATA INBOX (\"/shared/comment\" \"value\")\r\n"},
		{name: "SETMETADATA", start: func(c *imapclient.Client) func() error {
			value := []byte("value")
			return c.SetMetadata("inbox", map[string]*[]byte{"/shared/comment": &value}).Wait
		}},
		{name: "GETQUOTAROOT", start: func(c *imapclient.Client) func() error { return selectIsolation307Wait(c.GetQuotaRoot("INBOX").Wait) }, reply: "* QUOTAROOT INBOX \"\"\r\n* QUOTA \"\" (STORAGE 0 100)\r\n"},
		{name: "SUBSCRIBE", start: func(c *imapclient.Client) func() error { return c.Subscribe("inbox").Wait }},
		{name: "UNSUBSCRIBE", start: func(c *imapclient.Client) func() error { return c.Unsubscribe("inbox").Wait }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := tc.selected
			if selected == "" {
				selected = "INBOX"
			}
			var exists atomic.Uint32
			c, done := newSelectIsolation307Peer(t, "IMAP4rev1 ESEARCH MULTISEARCH CATENATE CONDSTORE ACL METADATA QUOTA LIST-STATUS", &imapclient.Options{
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
				if tc.delimiter != "" {
					if _, err := fmt.Fprintf(conn, "* LIST () %q %q\r\n", tc.delimiter, selected); err != nil {
						return err
					}
				}
				if err := writeSelectIsolation307Data(conn, tag, 1, `\Answered`, `\Answered`); err != nil {
					return err
				}
				previous, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				next, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprint(conn, "* 2 EXISTS\r\n"); err != nil {
					return err
				}
				if strings.HasPrefix(tc.name, "MULTISEARCH") {
					if _, err := fmt.Fprintf(conn, "* ESEARCH (TAG %q MAILBOX %q UIDVALIDITY 10) UID ALL 1:2\r\n", previous, selected); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintf(conn, "%s%s OK earlier command\r\n%s BAD invalid SELECT\r\n", tc.reply, previous, next); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			if _, err := c.Select(selected, nil).Wait(); err != nil {
				t.Fatal(err)
			}
			wait := tc.start(c)
			sel := c.Select("Other", &imap.SelectOptions{CondStore: true})
			if err := wait(); err != nil {
				t.Fatalf("earlier command: %v", err)
			}
			if _, err := sel.Wait(); err == nil {
				t.Fatal("expected SELECT BAD")
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := c.Mailbox(); got == nil || got.Name != selected || got.NumMessages != 2 {
				t.Errorf("old mailbox update was stolen by pending SELECT: %+v", got)
			}
			if got := exists.Load(); got != 2 {
				t.Errorf("unilateral EXISTS=%d, want 2", got)
			}
		})
	}
}

func TestSelectListResponseIsolation(t *testing.T) {
	for _, mode := range []string{"canonical INBOX", "canonical inbox", "rename before CLOSED", "OLDNAME while selecting"} {
		t.Run(mode, func(t *testing.T) {
			notifications := make(chan string, 3)
			c, done := newSelectIsolation307Peer(t, "IMAP4rev2 CONDSTORE", &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{List: func(data *imap.ListData) {
				notifications <- data.Mailbox + ":" + data.OldName
			}}}, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				mailbox := "INBOX"
				if mode == "rename before CLOSED" {
					if err := writeSelectIsolation307Data(conn, tag, 1, `\Answered`, `\Answered`); err != nil {
						return err
					}
					rename, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					tag, err = readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					if _, err := fmt.Fprintf(conn, "* LIST () \"/\" Archive (\"OLDNAME\" (\"Drafts\"))\r\n%s OK renamed\r\n* OK [CLOSED] old selection ended\r\n", rename); err != nil {
						return err
					}
					mailbox = "Archive"
				} else if mode == "OLDNAME while selecting" {
					if _, err := fmt.Fprint(conn, "* LIST () \"/\" Archive (\"OLDNAME\" (\"Drafts\"))\r\n"); err != nil {
						return err
					}
					mailbox = "Archive"
				}
				if _, err := fmt.Fprintf(conn, "* LIST () \"/\" %s\r\n", mailbox); err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 2, `\Seen`, `\Seen`); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			mailbox := "INBOX"
			var rename *imapclient.Command
			if mode == "rename before CLOSED" {
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					t.Fatal(err)
				}
				rename = c.Rename("Drafts", "Archive", nil)
				mailbox = "Archive"
			} else if mode == "OLDNAME while selecting" {
				mailbox = "Archive"
			} else if mode == "canonical inbox" {
				mailbox = "inbox"
			}
			selection := c.Select(mailbox, nil)
			if rename != nil {
				if err := rename.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			data, err := selection.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			wantMailbox := "INBOX"
			if strings.Contains(mode, "OLDNAME") || strings.Contains(mode, "rename") {
				wantMailbox = "Archive"
			}
			if data.List == nil || data.List.Mailbox != wantMailbox || data.List.OldName != "" {
				t.Errorf("SELECT lost its own LIST data: %+v", data.List)
			}
			if wantMailbox == "Archive" {
				select {
				case got := <-notifications:
					if got != "Archive:Drafts" {
						t.Errorf("OLDNAME callback=%q, want Archive:Drafts", got)
					}
				default:
					t.Error("OLDNAME notification was stolen by SELECT")
				}
			} else if len(notifications) != 0 {
				t.Errorf("SELECT LIST was reported as unsolicited: %q", <-notifications)
			}
		})
	}
}
