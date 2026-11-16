package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestSelectRefusedWithNOReplaysClosingMailboxUpdates(t *testing.T) {
	for _, examine := range []bool{false, true} {
		t.Run(fmt.Sprintf("examine=%v", examine), func(t *testing.T) {
			events := make(chan string, 16)
			var c *imapclient.Client
			snapshot := func() string {
				m := c.Mailbox()
				if m == nil {
					return "nil mailbox"
				}
				return fmt.Sprintf("box=%s count=%d flags=%v permanent=%v readOnly=%v",
					m.Name, m.NumMessages, m.Flags, m.PermanentFlags, m.ReadOnly)
			}

			options := &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Mailbox: func(data *imapclient.UnilateralDataMailbox) {
						if data.NumMessages != nil {
							events <- "EXISTS " + snapshot()
						} else if data.Flags != nil {
							events <- "FLAGS " + snapshot()
						} else if data.PermanentFlags != nil {
							events <- "PERMANENTFLAGS " + snapshot()
						}
					},
					Expunge: func(seq uint32) {
						events <- fmt.Sprintf("EXPUNGE(%d) %s", seq, snapshot())
					},
					List: func(data *imap.ListData) {
						events <- fmt.Sprintf("LIST(%s,%v) %s", data.Mailbox, data.Attrs, snapshot())
					},
				},
			}

			var done <-chan error
			c, done = newSelectIsolation307Peer(t, "IMAP4rev1 CONDSTORE", options, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 1, `\Answered`, `\Answered`); err != nil {
					return err
				}

				tag, err = readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn,
					"* 2 EXISTS\r\n"+
						"* FLAGS (\\Seen)\r\n"+
						"* OK [READ-ONLY] access changed\r\n"+
						"* 2 EXPUNGE\r\n"+
						"* OK [PERMANENTFLAGS (\\Seen)]\r\n"+
						"* LIST (\\Subscribed) \"/\" Missing\r\n"+
						"%s NO Mailbox does not exist\r\n", tag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})

			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			_, err := c.Select("Missing", &imap.SelectOptions{ReadOnly: examine, CondStore: true}).Wait()
			if err == nil {
				t.Fatal("expected SELECT NO error")
			}
			if c.State() != imap.ConnStateAuthenticated || c.Mailbox() != nil {
				t.Fatalf("after SELECT NO: state=%v mailbox=%+v, want Authenticated and nil", c.State(), c.Mailbox())
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			want := []string{
				`EXISTS box=Work count=2 flags=[\Answered] permanent=[\Answered] readOnly=false`,
				`FLAGS box=Work count=2 flags=[\Seen] permanent=[\Answered] readOnly=false`,
				`EXPUNGE(2) box=Work count=1 flags=[\Seen] permanent=[\Answered] readOnly=true`,
				`PERMANENTFLAGS box=Work count=1 flags=[\Seen] permanent=[\Seen] readOnly=true`,
				`LIST(Missing,[\Subscribed]) box=Work count=1 flags=[\Seen] permanent=[\Seen] readOnly=true`,
			}
			var got []string
			for range want {
				select {
				case ev := <-events:
					got = append(got, ev)
				case <-time.After(200 * time.Millisecond):
					got = append(got, "missing event")
				}
			}
			if !reflect.DeepEqual(got, want) || len(events) != 0 {
				t.Errorf("closing-mailbox callbacks before SELECT NO:\n got %q (+%d extra)\nwant %q", got, len(events), want)
			}
		})
	}
}

func TestPipelinedFetchDoesNotClaimPreSelectBoundary(t *testing.T) {
	for _, kind := range []string{"FETCH", "UID FETCH", "STORE", "UID STORE"} {
		t.Run(kind, func(t *testing.T) {
			unilateral := make(chan string, 4)
			c, done := newSelectIsolation307Peer(t, "IMAP4rev2", &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Fetch: func(msg *imapclient.FetchMessageData) {
						buf, err := msg.Collect()
						if err != nil {
							unilateral <- fmt.Sprintf("err:%v", err)
							return
						}
						unilateral <- fmt.Sprintf("seq=%d uid=%d flags=%v bodies=%d",
							buf.SeqNum, buf.UID, buf.Flags, len(buf.BodySection))
					},
				},
			}, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 3, `\Answered`, `\Answered`); err != nil {
					return err
				}

				selTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				cmdTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}

				if _, err := fmt.Fprint(conn,
					"* 1 FETCH (UID 1 FLAGS (\\Seen))\r\n"+
						"* OK [CLOSED] old selection ended\r\n"); err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, selTag, 2, `\Flagged`, `\Flagged`); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn,
					"* 1 FETCH (UID 1 FLAGS (\\Flagged) BODY[] {5}\r\nother)\r\n"+
						"%s OK command complete\r\n", cmdTag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})

			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}

			selCmd := c.Select("Other", nil)
			var numSet imap.NumSet = imap.SeqSetNum(1)
			if kind == "UID FETCH" || kind == "UID STORE" {
				numSet = imap.UIDSetNum(1)
			}
			var fetchCmd *imapclient.FetchCommand
			if kind == "STORE" || kind == "UID STORE" {
				fetchCmd = c.Store(numSet, &imap.StoreFlags{
					Op:    imap.StoreFlagsAdd,
					Flags: []imap.Flag{imap.FlagFlagged},
				}, nil)
			} else {
				fetchCmd = c.Fetch(numSet, &imap.FetchOptions{
					Flags:       true,
					BodySection: []*imap.FetchItemBodySection{{}},
				})
			}

			if _, err := selCmd.Wait(); err != nil {
				t.Fatal(err)
			}
			msgs, err := fetchCmd.Collect()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			if len(msgs) != 1 || !reflect.DeepEqual(msgs[0].Flags, []imap.Flag{imap.FlagFlagged}) ||
				len(msgs[0].BodySection) != 1 || string(msgs[0].BodySection[0].Bytes) != "other" {
				t.Fatalf("pipelined %s got wrong message data (stole pre-CLOSED old-mailbox FETCH): %+v", kind, msgs)
			}

			select {
			case got := <-unilateral:
				want := "seq=1 uid=1 flags=[\\Seen] bodies=0"
				if got != want {
					t.Errorf("unilateral Fetch = %q, want %q", got, want)
				}
			case <-time.After(200 * time.Millisecond):
				t.Error("pre-CLOSED unilateral FETCH for Work was lost")
			}
			if len(unilateral) != 0 {
				t.Errorf("extra unilateral Fetch callbacks: %d (%q)", len(unilateral), <-unilateral)
			}
		})
	}
}

func TestCapabilityUpdateMidSelectDoesNotStrandProvisionalJournal(t *testing.T) {
	events := make(chan string, 8)
	var c *imapclient.Client
	snapshot := func() string {
		m := c.Mailbox()
		if m == nil {
			return "nil mailbox"
		}
		return fmt.Sprintf("box=%s count=%d", m.Name, m.NumMessages)
	}
	var done <-chan error
	c, done = newSelectIsolation307Peer(t, "IMAP4rev1 IMAP4rev2", &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(d *imapclient.UnilateralDataMailbox) {
				if d.NumMessages != nil {
					events <- fmt.Sprintf("EXISTS(%d) %s", *d.NumMessages, snapshot())
				}
			},
			Expunge: func(seq uint32) {
				events <- fmt.Sprintf("EXPUNGE(%d) %s", seq, snapshot())
			},
		},
	}, func(br *bufio.Reader, conn net.Conn) error {
		tag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if err := writeSelectIsolation307Data(conn, tag, 3, `\Answered`, `\Answered`); err != nil {
			return err
		}

		selTag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		capTag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(conn,
			"* 5 EXISTS\r\n"+
				"* CAPABILITY IMAP4rev2\r\n"+
				"%s OK capability done\r\n"+
				"* 2 EXPUNGE\r\n"+
				"* OK [CLOSED] old selection ended\r\n", capTag); err != nil {
			return err
		}
		if err := writeSelectIsolation307Data(conn, selTag, 7, `\Flagged`, `\Flagged`); err != nil {
			return err
		}
		return finishSelectIsolation307Noop(br, conn)
	})

	if _, err := c.Select("Work", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	selCmd := c.Select("Other", nil)
	capCmd := c.Capability()
	data, err := selCmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capCmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if data.NumMessages != 7 || c.Mailbox().Name != "Other" || c.Mailbox().NumMessages != 7 {
		t.Fatalf("Other selection = %+v / %+v", data, c.Mailbox())
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	want := []string{
		"EXISTS(5) box=Work count=5",
		"EXPUNGE(2) box=Work count=4",
	}
	var got []string
	for range want {
		select {
		case ev := <-events:
			got = append(got, ev)
		case <-time.After(200 * time.Millisecond):
			got = append(got, "missing event")
		}
	}
	if !reflect.DeepEqual(got, want) || len(events) != 0 {
		t.Errorf("events = %q (+%d extra), want %q", got, len(events), want)
	}
}
