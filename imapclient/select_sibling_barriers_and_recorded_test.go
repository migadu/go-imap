package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestPipelinedSelectedCommandsDoNotClaimPreSelectBoundary(t *testing.T) {
	for _, barrier := range []string{"SELECT", "UNSELECT", "CLOSE", "UNAUTHENTICATE"} {
		t.Run("barrier="+barrier+"/UIDSearch_plain", func(t *testing.T) {
			c, done := newSelectIsolation307Peer(t, "IMAP4rev2 UNAUTHENTICATE", nil, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 3, `\Answered`, `\Answered`); err != nil {
					return err
				}

				barrierTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				cmdTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}

				// Pre-boundary * SEARCH emitted before the selection transition completes.
				if _, err := fmt.Fprint(conn, "* SEARCH 99 (MODSEQ 500)\r\n"); err != nil {
					return err
				}
				switch barrier {
				case "SELECT":
					if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
						return err
					}
					if err := writeSelectIsolation307Data(conn, barrierTag, 2, `\Flagged`, `\Flagged`); err != nil {
						return err
					}
					if _, err := fmt.Fprintf(conn, "* SEARCH 1 2\r\n%s OK UID SEARCH completed\r\n", cmdTag); err != nil {
						return err
					}
				case "UNSELECT", "CLOSE", "UNAUTHENTICATE":
					if _, err := fmt.Fprintf(conn, "%s OK %s completed\r\n%s OK completed\r\n", barrierTag, barrier, cmdTag); err != nil {
						return err
					}
				}

				// Answer trailing commands (including UNAUTHENTICATE's auto CAPABILITY refresh) up to NOOP.
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return err
					}
					tTag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
					if rest == "CAPABILITY" {
						if _, err := fmt.Fprintf(conn, "* CAPABILITY IMAP4rev2 UNAUTHENTICATE\r\n%s OK capability\r\n", tTag); err != nil {
							return err
						}
						continue
					}
					_, err = fmt.Fprintf(conn, "%s OK noop\r\n", tTag)
					return err
				}
			})

			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			var waitBarrier func() error
			switch barrier {
			case "SELECT":
				selCmd := c.Select("Other", nil)
				waitBarrier = func() error { _, err := selCmd.Wait(); return err }
			case "UNSELECT":
				waitBarrier = c.Unselect().Wait
			case "CLOSE":
				waitBarrier = c.UnselectAndExpunge().Wait
			case "UNAUTHENTICATE":
				waitBarrier = c.Unauthenticate().Wait
			}
			searchCmd := c.UIDSearch(&imap.SearchCriteria{}, nil)

			if err := waitBarrier(); err != nil {
				t.Fatal(err)
			}
			data, err := searchCmd.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			uidSet, _ := data.All.(imap.UIDSet)
			nums, _ := uidSet.Nums()
			var want []imap.UID
			if barrier == "SELECT" {
				want = []imap.UID{1, 2}
			}
			if !reflect.DeepEqual(nums, want) || data.ModSeq != 0 {
				t.Fatalf("pipelined UID SEARCH stole pre-%s response: nums=%v modseq=%d, want nums=%v modseq=0", barrier, nums, data.ModSeq, want)
			}
		})
	}

	t.Run("UIDSearch_ESEARCH", func(t *testing.T) {
		c, done := newSelectIsolation307Peer(t, "IMAP4rev2", nil, func(br *bufio.Reader, conn net.Conn) error {
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

			if _, err := fmt.Fprintf(conn,
				"* ESEARCH (TAG %q) UID ALL 99\r\n"+
					"* OK [CLOSED] old selection ended\r\n", cmdTag); err != nil {
				return err
			}
			if err := writeSelectIsolation307Data(conn, selTag, 0, `\Flagged`, `\Flagged`); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK UID SEARCH completed\r\n", cmdTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		if _, err := c.Select("Work", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		selCmd := c.Select("Other", nil)
		searchCmd := c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true})

		if _, err := selCmd.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := searchCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		uidSet, _ := data.All.(imap.UIDSet)
		if len(uidSet) != 0 {
			t.Fatalf("pipelined UID SEARCH (ESEARCH) stole pre-SELECT response: All=%v, want empty", data.All)
		}
	})

	t.Run("UIDSort_plain_and_ESORT", func(t *testing.T) {
		for _, resp := range []string{"* SORT 99\r\n", "* ESORT ALL 99\r\n"} {
			t.Run(resp[:6], func(t *testing.T) {
				c, done := newSelectIsolation307Peer(t, "IMAP4rev2 SORT ESORT", nil, func(br *bufio.Reader, conn net.Conn) error {
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

					if _, err := fmt.Fprint(conn, resp+"* OK [CLOSED] old selection ended\r\n"); err != nil {
						return err
					}
					if err := writeSelectIsolation307Data(conn, selTag, 2, `\Flagged`, `\Flagged`); err != nil {
						return err
					}
					if _, err := fmt.Fprintf(conn,
						"* SORT 2 1\r\n"+
							"%s OK UID SORT completed\r\n", cmdTag); err != nil {
						return err
					}
					return finishSelectIsolation307Noop(br, conn)
				})

				if _, err := c.Select("Work", nil).Wait(); err != nil {
					t.Fatal(err)
				}
				selCmd := c.Select("Other", nil)
				sortCmd := c.UIDSort(&imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDate}},
					SearchCriteria: &imap.SearchCriteria{},
				})

				if _, err := selCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := sortCmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}

				want := []imap.UID{2, 1}
				if !reflect.DeepEqual(data.UIDs, want) {
					t.Fatalf("pipelined UID SORT stole pre-SELECT response: UIDs=%v, want %v", data.UIDs, want)
				}
			})
		}
	})

	t.Run("UIDSort_ESEARCH", func(t *testing.T) {
		c, done := newSelectIsolation307Peer(t, "IMAP4rev2 SORT ESORT", nil, func(br *bufio.Reader, conn net.Conn) error {
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

			if _, err := fmt.Fprintf(conn,
				"* ESEARCH (TAG %q) UID ALL 99 MODSEQ 500\r\n"+
					"* OK [CLOSED] old selection ended\r\n", cmdTag); err != nil {
				return err
			}
			if err := writeSelectIsolation307Data(conn, selTag, 0, `\Flagged`, `\Flagged`); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn,
				"* ESEARCH (TAG %q) UID COUNT 0\r\n"+
					"%s OK UID SORT completed\r\n", cmdTag, cmdTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		if _, err := c.Select("Work", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		selCmd := c.Select("Other", nil)
		sortCmd := c.UIDSort(&imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDate}},
			SearchCriteria: &imap.SearchCriteria{},
			Return:         imap.SortOptions{ReturnAll: true, ReturnCount: true},
		})

		if _, err := selCmd.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := sortCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if len(data.UIDs) != 0 || data.ModSeq != 0 {
			t.Fatalf("pipelined UID SORT (ESEARCH) stole pre-SELECT response: UIDs=%v modseq=%d, want empty", data.UIDs, data.ModSeq)
		}
	})

	t.Run("UIDThread", func(t *testing.T) {
		c, done := newSelectIsolation307Peer(t, "IMAP4rev2 THREAD=REFERENCES", nil, func(br *bufio.Reader, conn net.Conn) error {
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
				"* THREAD (99)\r\n"+
					"* OK [CLOSED] old selection ended\r\n"); err != nil {
				return err
			}
			if err := writeSelectIsolation307Data(conn, selTag, 2, `\Flagged`, `\Flagged`); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn,
				"* THREAD (1 2)\r\n"+
					"%s OK UID THREAD completed\r\n", cmdTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		if _, err := c.Select("Work", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		selCmd := c.Select("Other", nil)
		threadCmd := c.UIDThread(&imapclient.ThreadOptions{
			Algorithm:      imap.ThreadReferences,
			SearchCriteria: &imap.SearchCriteria{},
		})

		if _, err := selCmd.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := threadCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		want := []imap.ThreadData{{Chain: []uint32{1, 2}}}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("pipelined UID THREAD stole pre-SELECT response: got=%+v, want=%+v", data, want)
		}
	})

	t.Run("UIDMove_COPYUID", func(t *testing.T) {
		c, done := newSelectIsolation307Peer(t, "IMAP4rev2 MOVE UIDPLUS", nil, func(br *bufio.Reader, conn net.Conn) error {
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
				"* OK [COPYUID 10 1 99] prior move\r\n"+
					"* OK [CLOSED] old selection ended\r\n"); err != nil {
				return err
			}
			if err := writeSelectIsolation307Data(conn, selTag, 0, `\Flagged`, `\Flagged`); err != nil {
				return err
			}
			// UID 1 does not exist in Other, so UID MOVE succeeds with OK and no COPYUID.
			if _, err := fmt.Fprintf(conn, "%s OK UID MOVE completed\r\n", cmdTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		// Prime capabilities before pipelining Move (Move calls c.Caps()).
		_ = c.Caps()
		if _, err := c.Select("Work", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		selCmd := c.Select("Other", nil)
		moveCmd := c.Move(imap.UIDSetNum(1), "Archive")

		if _, err := selCmd.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := moveCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if data.UIDValidity != 0 || len(data.UIDMapping) != 0 {
			t.Fatalf("pipelined UID MOVE stole pre-SELECT COPYUID: %+v, want empty", data)
		}
	})
}

func TestMidSelectEnableOrCapsAfterProvisionalSelectDataDoesNotSplitSelect(t *testing.T) {
	for _, trigger := range []string{"ENABLED", "CAPABILITY"} {
		t.Run(trigger, func(t *testing.T) {
			events := make(chan string, 8)
			c, done := newSelectIsolation307Peer(t, "IMAP4rev1 IMAP4rev2 ENABLE CONDSTORE", &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Mailbox: func(d *imapclient.UnilateralDataMailbox) {
						events <- fmt.Sprintf("%+v", d)
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
				secondTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}

				midLine := "* ENABLED IMAP4rev2\r\n" + secondTag + " OK enabled\r\n"
				if trigger == "CAPABILITY" {
					midLine = "* CAPABILITY IMAP4rev2 CONDSTORE\r\n" + secondTag + " OK capability done\r\n"
				}
				if _, err := fmt.Fprintf(conn,
					"* OK [UIDVALIDITY 20] validity\r\n"+
						"* OK [UIDNEXT 8] next\r\n"+
						"* OK [HIGHESTMODSEQ 77] modseq\r\n"+
						"%s"+
						"* 7 EXISTS\r\n"+
						"* FLAGS (\\Flagged)\r\n"+
						"* OK [PERMANENTFLAGS (\\Flagged)] flags\r\n"+
						"%s OK [READ-WRITE] selected\r\n", midLine, selTag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})

			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			selCmd := c.Select("Other", &imap.SelectOptions{CondStore: true})
			var waitSecond func() error
			if trigger == "ENABLED" {
				enCmd := c.Enable(imap.CapIMAP4rev2)
				waitSecond = func() error { _, err := enCmd.Wait(); return err }
			} else {
				capCmd := c.Capability()
				waitSecond = func() error { _, err := capCmd.Wait(); return err }
			}

			data, err := selCmd.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if err := waitSecond(); err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			if data.UIDValidity != 20 || data.UIDNext != 8 || data.HighestModSeq != 77 ||
				data.NumMessages != 7 || !reflect.DeepEqual(data.Flags, []imap.Flag{imap.FlagFlagged}) ||
				!reflect.DeepEqual(data.PermanentFlags, []imap.Flag{imap.FlagFlagged}) {
				t.Fatalf("Select(Other) split mid-flight: got %+v", data)
			}
			mbox := c.Mailbox()
			if mbox == nil || mbox.Name != "Other" || mbox.NumMessages != 7 ||
				!reflect.DeepEqual(mbox.Flags, []imap.Flag{imap.FlagFlagged}) {
				t.Fatalf("c.Mailbox() lost post-%s opening data: %+v", trigger, mbox)
			}
			if len(events) != 0 {
				t.Fatalf("Other's opening responses were misrouted as Work unilateral callbacks: %d (%s)", len(events), <-events)
			}
		})
	}
}
