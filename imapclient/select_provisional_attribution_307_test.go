package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestSameMailboxSelectReplaysProvisionalList(t *testing.T) {
	for _, boundary := range []string{"CLOSED", "BAD", "omitted"} {
		t.Run(boundary, func(t *testing.T) {
			events := make(chan *imap.ListData, 4)
			c, done := newSelectIsolation307Peer(t, "IMAP4rev2", &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{List: func(d *imap.ListData) { events <- d }}}, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 3, `\Answered`, `\Answered`); err != nil {
					return err
				}
				tag, err = readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprint(conn, "* LIST (\\Subscribed) \"/\" Work\r\n"); err != nil {
					return err
				}
				if boundary == "BAD" {
					_, err = fmt.Fprintf(conn, "%s BAD invalid selection\r\n", tag)
				} else {
					if boundary == "CLOSED" {
						if _, err := fmt.Fprint(conn, "* OK [CLOSED] old ended\r\n* LIST (\\HasNoChildren) \"/\" Work\r\n"); err != nil {
							return err
						}
					}
					err = writeSelectIsolation307Data(conn, tag, 7, `\Flagged`, `\Flagged`)
				}
				if err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := c.Select("Work", nil).Wait()
			if (err != nil) != (boundary == "BAD") {
				t.Fatalf("error=%v", err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if boundary == "omitted" {
				if len(events) != 0 {
					t.Fatal("opening LIST delivered unilaterally")
				}
				if data.List == nil || !reflect.DeepEqual(data.List.Attrs, []imap.MailboxAttr{imap.MailboxAttrSubscribed}) {
					t.Fatalf("SelectData.List=%+v", data.List)
				}
			} else {
				select {
				case d := <-events:
					if !reflect.DeepEqual(d.Attrs, []imap.MailboxAttr{imap.MailboxAttrSubscribed}) {
						t.Fatalf("old LIST=%+v", d)
					}
				case <-time.After(100 * time.Millisecond):
					t.Fatal("pre-boundary normal LIST lost")
				}
				if boundary == "CLOSED" && (data.List == nil || !reflect.DeepEqual(data.List.Attrs, []imap.MailboxAttr{imap.MailboxAttrHasNoChildren})) {
					t.Fatalf("new LIST=%+v", data.List)
				}
			}
		})
	}
}

func TestPipelinedSelectBADIsolatesFollowingSelection(t *testing.T) {
	for _, secondClosed := range []bool{false, true} {
		t.Run(fmt.Sprint(secondClosed), func(t *testing.T) {
			events := make(chan uint32, 8)
			c, done := newSelectIsolation307Peer(t, "IMAP4rev1", &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					events <- *d.NumMessages
				}
			}}}, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 3, `\Seen`, `\Seen`); err != nil {
					return err
				}
				first, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				second, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "* 5 EXISTS\r\n%s BAD invalid first\r\n", first); err != nil {
					return err
				}
				if secondClosed {
					if _, err := fmt.Fprint(conn, "* OK [CLOSED] old ended\r\n"); err != nil {
						return err
					}
				}
				if err := writeSelectIsolation307Data(conn, second, 7, `\Flagged`, `\Flagged`); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			first, second := c.Select("Work", nil), c.Select("Work", nil)
			if _, err := first.Wait(); err == nil || !strings.Contains(err.Error(), "invalid first") {
				t.Fatalf("first=%v", err)
			}
			data, err := second.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if data.NumMessages != 7 || c.Mailbox().NumMessages != 7 {
				t.Fatalf("second data=%+v mailbox=%+v", data, c.Mailbox())
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || <-events != 5 {
				t.Fatal("old count not delivered exactly once")
			}
		})
	}
}

// An EXPUNGE queued after SELECT cannot own updates sent for the old selection
// before CLOSED. Otherwise it also overtakes the old selection's queued EXISTS.
func TestPipelinedExpungeDoesNotClaimPreSelectBoundary(t *testing.T) {
	events := make(chan string, 4)
	var c *imapclient.Client
	var done <-chan error
	c, done = newSelectIsolation307Peer(t, "IMAP4rev1 CONDSTORE", &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil {
				events <- fmt.Sprintf("EXISTS %d/%d", *d.NumMessages, c.Mailbox().NumMessages)
			}
		},
		Expunge: func(seq uint32) { events <- fmt.Sprintf("EXPUNGE %d/%d", seq, c.Mailbox().NumMessages) },
	}}, func(br *bufio.Reader, conn net.Conn) error {
		tag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if err := writeSelectIsolation307Data(conn, tag, 3, `\Answered`, `\Answered`); err != nil {
			return err
		}
		selection, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		expunge, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprint(conn, "* 5 EXISTS\r\n* 2 EXPUNGE\r\n* OK [CLOSED] old selection ended\r\n"); err != nil {
			return err
		}
		if err := writeSelectIsolation307Data(conn, selection, 7, `\Flagged`, `\Flagged`); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "%s OK expunge complete\r\n", expunge); err != nil {
			return err
		}
		return finishSelectIsolation307Noop(br, conn)
	})
	if _, err := c.Select("Work", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	selection := c.Select("Work", &imap.SelectOptions{CondStore: true})
	expunge := c.Expunge()
	data, err := selection.Wait()
	if err != nil {
		t.Fatal(err)
	}
	seqs, err := expunge.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 0 {
		t.Errorf("later EXPUNGE stole old mailbox response: %v", seqs)
	}
	if data.NumMessages != 7 || c.Mailbox().NumMessages != 7 {
		t.Errorf("opening count changed: data=%+v mailbox=%+v", data, c.Mailbox())
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var got []string
	for range 2 {
		select {
		case event := <-events:
			got = append(got, event)
		case <-time.After(100 * time.Millisecond):
			got = append(got, "missing event")
		}
	}
	if want := []string{"EXISTS 5/5", "EXPUNGE 2/4"}; !reflect.DeepEqual(got, want) || len(events) != 0 {
		t.Errorf("events=%v (+%d extra), want %v", got, len(events), want)
	}
}

// RFC 9051 section 7.1 allows READ-ONLY to report a change to the selected
// mailbox's access. Even on an independent command's tagged completion, that
// change must remain ordered with earlier deferred mailbox updates.
func TestTaggedReadOnlyStaysOrderedWithProvisionalUpdates(t *testing.T) {
	for _, boundary := range []string{"CLOSED", "BAD"} {
		t.Run(boundary, func(t *testing.T) {
			events := make(chan string, 4)
			var c *imapclient.Client
			var done <-chan error
			c, done = newSelectIsolation307Peer(t, "IMAP4rev1 CONDSTORE", &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Mailbox: func(d *imapclient.UnilateralDataMailbox) {
					if d.NumMessages != nil {
						events <- fmt.Sprintf("EXISTS readOnly=%v", c.Mailbox().ReadOnly)
					} else if d.Flags != nil {
						events <- fmt.Sprintf("FLAGS readOnly=%v", c.Mailbox().ReadOnly)
					}
				},
			}}, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 3, `\Answered`, `\Answered`); err != nil {
					return err
				}
				selection, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				capability, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "* 5 EXISTS\r\n* CAPABILITY IMAP4rev1 CONDSTORE\r\n%s OK [READ-ONLY] access changed\r\n* FLAGS (\\Seen)\r\n", capability); err != nil {
					return err
				}
				if boundary == "BAD" {
					_, err = fmt.Fprintf(conn, "%s BAD invalid selection\r\n", selection)
				} else {
					if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
						return err
					}
					err = writeSelectIsolation307Data(conn, selection, 7, `\Flagged`, `\Flagged`)
				}
				if err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			selection := c.Select("Work", nil)
			capability := c.Capability()
			if _, err := selection.Wait(); (err != nil) != (boundary == "BAD") {
				t.Fatalf("selection error=%v", err)
			}
			if _, err := capability.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var got []string
			for len(events) > 0 {
				got = append(got, <-events)
			}
			if want := []string{"EXISTS readOnly=false", "FLAGS readOnly=true"}; !reflect.DeepEqual(got, want) {
				t.Errorf("events=%v, want %v", got, want)
			}
			if c.Mailbox().ReadOnly != (boundary == "BAD") {
				t.Errorf("final readOnly=%v, boundary=%s", c.Mailbox().ReadOnly, boundary)
			}
		})
	}
}

func TestSuccessfulNoClosedSelectReleasesProvisionalFetchBudget(t *testing.T) {
	const rounds = 10
	body := strings.Repeat("b", 1024)
	fetches := make(chan struct{}, rounds*2)
	c, done := newSelectIsolation307Peer(t, "IMAP4rev1 ENABLE CONDSTORE QRESYNC", &imapclient.Options{
		MaxFetchBufferSize:    4096,
		MaxLiteralSize:        2048,
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{Fetch: func(msg *imapclient.FetchMessageData) { _, _ = msg.Collect(); fetches <- struct{}{} }},
	}, func(br *bufio.Reader, conn net.Conn) error {
		if err := journal307Opening(br, conn, true, 3); err != nil {
			return err
		}
		for round := 0; round < rounds; round++ {
			tag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			for uid := 1; uid <= 2; uid++ {
				if _, err := fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (\\Seen) MODSEQ (2) BODY[] {1024}\r\n%s)\r\n", uid, uid, body); err != nil {
					return err
				}
			}
			if err := writeSelectIsolation307Data(conn, tag, 3, `\Seen`, `\Seen`); err != nil {
				return err
			}
		}
		tag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		// This ordinary UID FETCH must buffer its body until the trailing UID.
		if _, err := fmt.Fprintf(conn, "* 1 FETCH (BODY[] {1024}\r\n%s UID 1)\r\n%s OK fetched\r\n", body, tag); err != nil {
			return err
		}
		return finishSelectIsolation307Noop(br, conn)
	})
	journal307StartSelection(t, c, true)
	for round := 0; round < rounds; round++ {
		data, err := c.Select("Work", &imap.SelectOptions{QResync: &imap.QResyncData{UIDValidity: 10, ModSeq: 1}}).Wait()
		if err != nil {
			t.Fatalf("selection round %d exhausted reused budget: %v", round, err)
		}
		if len(data.Modified) != 2 {
			t.Errorf("round %d: Modified=%d, want 2", round, len(data.Modified))
		}
	}
	data, err := c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	if err != nil {
		t.Fatalf("UID FETCH after selections: %v", err)
	}
	if len(data) != 1 || data[0].UID != 1 || len(data[0].BodySection) != 1 || string(data[0].BodySection[0].Bytes) != body {
		t.Fatalf("UID FETCH data=%+v", data)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(fetches) != 0 {
		t.Errorf("no-CLOSED successful SELECT delivered %d provisional FETCH callbacks", len(fetches))
	}
}
