package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 5182 §2.1, RFC 4731 §3.1, and RFC 9051 §6.4.4: a SEARCH command carrying
// RETURN (SAVE) (or any non-empty RETURN option list) never receives an
// untagged * SEARCH response — SAVE alone suppresses all untagged responses,
// and combined options are answered exclusively via tag-correlated * ESEARCH.
// When pipelined ahead of a plain SEARCH / UID SEARCH (or when an unsolicited
// * SEARCH arrives), handleSearch must not attribute * SEARCH to the extended
// command or mutate an already-delivered ESEARCH result.
func TestExtendedSearchDoesNotClaimPlainSearchResponse(t *testing.T) {
	t.Run("save_only_pipelined_before_split_plain_uid_search", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		done := make(chan error, 1)
		go func() {
			done <- func() error {
				br := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ESEARCH SEARCHRES] ready\r\n"); err != nil {
					return err
				}
				line, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				selTag, _, _ := strings.Cut(line, " ")
				if _, err := fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
					return err
				}
				line1, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag1, cmd1, _ := strings.Cut(strings.TrimSuffix(line1, "\r\n"), " ")
				if cmd1 != "UID SEARCH RETURN (SAVE) FLAGGED" {
					return fmt.Errorf("cmd1 = %q", cmd1)
				}
				line2, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag2, cmd2, _ := strings.Cut(strings.TrimSuffix(line2, "\r\n"), " ")
				if cmd2 != "UID SEARCH UNSEEN" {
					return fmt.Errorf("cmd2 = %q", cmd2)
				}
				_, err = fmt.Fprintf(serverConn, "* SEARCH 42\r\n* SEARCH 43\r\n%s OK saved\r\n%s OK searched\r\n", tag1, tag2)
				return err
			}()
		}()

		c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		saveCmd := c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, &imap.SearchOptions{ReturnSave: true})
		plainCmd := c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil)

		saveData, err := saveCmd.Wait()
		if err != nil {
			t.Fatalf("saveCmd.Wait: %v", err)
		}
		plainData, err := plainCmd.Wait()
		if err != nil {
			t.Fatalf("plainCmd.Wait: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if got := saveData.AllUIDs(); len(got) != 0 {
			t.Errorf("SAVE-only UIDSearch stole * SEARCH response: AllUIDs() = %v, want empty", got)
		}
		if got, want := plainData.AllUIDs(), []imap.UID{42, 43}; !slices.Equal(got, want) {
			t.Errorf("plain UIDSearch AllUIDs() = %v, want %v", got, want)
		}
	})

	t.Run("seq_save_only_pipelined_before_plain_uid_search", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		done := make(chan error, 1)
		go func() {
			done <- func() error {
				br := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ESEARCH SEARCHRES] ready\r\n"); err != nil {
					return err
				}
				line, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				selTag, _, _ := strings.Cut(line, " ")
				if _, err := fmt.Fprintf(serverConn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
					return err
				}
				line1, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag1, _, _ := strings.Cut(line1, " ")
				line2, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag2, _, _ := strings.Cut(line2, " ")
				_, err = fmt.Fprintf(serverConn, "* SEARCH 500\r\n%s OK saved\r\n%s OK searched\r\n", tag1, tag2)
				return err
			}()
		}()

		c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		saveCmd := c.Search(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, &imap.SearchOptions{ReturnSave: true})
		plainCmd := c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil)

		saveData, err := saveCmd.Wait()
		if err != nil {
			t.Fatalf("saveCmd.Wait: %v", err)
		}
		plainData, err := plainCmd.Wait()
		if err != nil {
			t.Fatalf("plainCmd.Wait: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if got := saveData.AllSeqNums(); len(got) != 0 {
			t.Errorf("SAVE-only Search corrupted UID into sequence numbers: AllSeqNums() = %v, want empty", got)
		}
		if got, want := plainData.AllUIDs(), []imap.UID{500}; !slices.Equal(got, want) {
			t.Errorf("plain UIDSearch AllUIDs() = %v, want %v", got, want)
		}
	})

	t.Run("save_and_count_pipelined_before_plain_uid_search", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		done := make(chan error, 1)
		go func() {
			done <- func() error {
				br := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ESEARCH SEARCHRES] ready\r\n"); err != nil {
					return err
				}
				line, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				selTag, _, _ := strings.Cut(line, " ")
				if _, err := fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
					return err
				}
				line1, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag1, _, _ := strings.Cut(line1, " ")
				line2, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag2, _, _ := strings.Cut(line2, " ")
				_, err = fmt.Fprintf(serverConn, "* SEARCH 42\r\n* ESEARCH (TAG %q) UID COUNT 2\r\n%s OK saved\r\n%s OK searched\r\n", tag1, tag1, tag2)
				return err
			}()
		}()

		c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		saveCmd := c.UIDSearch(&imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, &imap.SearchOptions{ReturnSave: true, ReturnCount: true})
		plainCmd := c.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil)

		saveData, err := saveCmd.Wait()
		if err != nil {
			t.Fatalf("saveCmd.Wait: %v", err)
		}
		plainData, err := plainCmd.Wait()
		if err != nil {
			t.Fatalf("plainCmd.Wait: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if saveData.Count != 2 || len(saveData.AllUIDs()) != 0 {
			t.Errorf("saveCmd data = %+v, want Count=2 and empty All", saveData)
		}
		if got, want := plainData.AllUIDs(), []imap.UID{42}; !slices.Equal(got, want) {
			t.Errorf("plain UIDSearch result vanished: AllUIDs() = %v, want %v", got, want)
		}
	})

	t.Run("save_only_ignores_unsolicited_search", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		done := make(chan error, 1)
		go func() {
			done <- func() error {
				br := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ESEARCH SEARCHRES CONDSTORE] ready\r\n"); err != nil {
					return err
				}
				line, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				selTag, _, _ := strings.Cut(line, " ")
				if _, err := fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
					return err
				}
				line1, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag1, _, _ := strings.Cut(line1, " ")
				_, err = fmt.Fprintf(serverConn, "* SEARCH 99 (MODSEQ 7)\r\n%s OK saved\r\n", tag1)
				return err
			}()
		}()

		c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		saveData, err := c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnSave: true}).Wait()
		if err != nil {
			t.Fatalf("saveCmd.Wait: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(saveData.AllUIDs()) != 0 || saveData.ModSeq != 0 {
			t.Errorf("SAVE-only UIDSearch absorbed * SEARCH: AllUIDs=%v ModSeq=%d, want empty and 0", saveData.AllUIDs(), saveData.ModSeq)
		}
	})

	t.Run("rev2_esearch_result_not_mutated_by_stray_search", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		done := make(chan error, 1)
		go func() {
			done <- func() error {
				br := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n"); err != nil {
					return err
				}
				line, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				selTag, _, _ := strings.Cut(line, " ")
				if _, err := fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
					return err
				}
				line1, err := br.ReadString('\n')
				if err != nil {
					return err
				}
				tag1, _, _ := strings.Cut(line1, " ")
				_, err = fmt.Fprintf(serverConn, "* ESEARCH (TAG %q) UID ALL 10,20\r\n* SEARCH 99\r\n%s OK searched\r\n", tag1, tag1)
				return err
			}()
		}()

		c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		data, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			t.Fatalf("UIDSearch.Wait: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got, want := data.AllUIDs(), []imap.UID{10, 20}; !slices.Equal(got, want) {
			t.Errorf("ESEARCH-answered UIDSearch mutated by stray * SEARCH: AllUIDs() = %v, want %v", got, want)
		}
	})
}

// RFC 5267 §3.1: an extended SORT (with RETURN options) is answered via ESEARCH,
// never * SORT. Pipelining an extended SORT ahead of a plain SORT must not let
// handleSort steal * SORT into the extended command or leak unrequested UIDs.
func TestExtendedSortDoesNotClaimPlainSortResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

	done := make(chan error, 1)
	go func() {
		done <- func() error {
			br := bufio.NewReader(serverConn)
			if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 SORT ESORT ESEARCH] ready\r\n"); err != nil {
				return err
			}
			line, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			selTag, _, _ := strings.Cut(line, " ")
			if _, err := fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
				return err
			}
			line1, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			tag1, _, _ := strings.Cut(line1, " ")
			line2, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			tag2, _, _ := strings.Cut(line2, " ")
			_, err = fmt.Fprintf(serverConn, "* SORT 30 10\r\n* ESEARCH (TAG %q) UID COUNT 2\r\n%s OK esort\r\n%s OK sort\r\n", tag1, tag1, tag2)
			return err
		}()
	}()

	c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	extCmd := c.UIDSort(&imapclient.SortOptions{
		SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
		SearchCriteria: &imap.SearchCriteria{},
		Return:         imap.SortOptions{ReturnCount: true},
	})
	plainCmd := c.UIDSort(&imapclient.SortOptions{
		SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: true}},
		SearchCriteria: &imap.SearchCriteria{},
	})

	extData, err := extCmd.Wait()
	if err != nil {
		t.Fatalf("extCmd.Wait: %v", err)
	}
	plainData, err := plainCmd.Wait()
	if err != nil {
		t.Fatalf("plainCmd.Wait: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if extData.Count != 2 || len(extData.UIDs) != 0 || len(extData.SeqNums) != 0 {
		t.Errorf("extended UIDSort data = %+v, want Count=2 and empty UIDs/SeqNums", extData)
	}
	if want := []imap.UID{30, 10}; !slices.Equal(plainData.UIDs, want) {
		t.Errorf("plain UIDSort UIDs = %v, want %v", plainData.UIDs, want)
	}
}

// RFC 5182 §2.1 and RFC 7377 §2.2: SAVE operates on the selected mailbox's "$"
// variable and requires Selected state, even if a caller passes a non-Selected
// SearchSource to MultiSearch.
func TestMultiSearchReturnSaveRespectsSelectionBarriers(t *testing.T) {
	for _, barrier := range []string{"SELECT", "UNSELECT", "CLOSE"} {
		t.Run("preceding_"+barrier, func(t *testing.T) {
			c, done := newSelectIsolation307Peer(t, "IMAP4rev2 ESEARCH SEARCHRES MULTISEARCH", nil, func(br *bufio.Reader, conn net.Conn) error {
				initialTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, initialTag, 3, `\Seen`, `\Seen`); err != nil {
					return err
				}
				barrierTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				searchTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "* ESEARCH (TAG %q MAILBOX \"Archive\" UIDVALIDITY 7) UID ALL 31\r\n", searchTag); err != nil {
					return err
				}
				if barrier == "SELECT" {
					if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
						return err
					}
					if err := writeSelectIsolation307Data(conn, barrierTag, 2, `\Flagged`, `\Flagged`); err != nil {
						return err
					}
				} else if _, err := fmt.Fprintf(conn, "%s OK %s completed\r\n", barrierTag, barrier); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "%s OK ESEARCH completed\r\n", searchTag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})

			if _, err := c.Select("Old", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			var waitBarrier func() error
			switch barrier {
			case "SELECT":
				cmd := c.Select("New", nil)
				waitBarrier = func() error { _, err := cmd.Wait(); return err }
			case "UNSELECT":
				waitBarrier = c.Unselect().Wait
			case "CLOSE":
				waitBarrier = c.UnselectAndExpunge().Wait
			}
			msCmd := c.MultiSearch(
				&imap.SearchSource{Mailboxes: []string{"Archive"}},
				&imap.SearchCriteria{},
				&imap.SearchOptions{ReturnSave: true, ReturnAll: true},
			)
			if err := waitBarrier(); err != nil {
				t.Fatal(err)
			}
			results, err := msCmd.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 0 {
				t.Errorf("MultiSearch with ReturnSave crossed %s barrier: got %+v, want empty", barrier, results)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("keeps_old_mailbox_updates_ahead_of_queued_select", func(t *testing.T) {
		c, done := newSelectIsolation307Peer(t, "IMAP4rev1 CONDSTORE ESEARCH SEARCHRES MULTISEARCH", nil, func(br *bufio.Reader, conn net.Conn) error {
			initialTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			if err := writeSelectIsolation307Data(conn, initialTag, 3, `\Seen`, `\Seen`); err != nil {
				return err
			}
			msTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			selTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "* OK [HIGHESTMODSEQ 999] old mailbox modseq\r\n%s BAD [CLIENTBUG] ESEARCH SAVE requires selected\r\n", msTag); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "* 2 EXISTS\r\n* FLAGS (\\Flagged)\r\n* OK [PERMANENTFLAGS (\\Flagged)] flags\r\n* OK [UIDVALIDITY 10] validity\r\n* OK [UIDNEXT 4] next\r\n%s OK [READ-WRITE] selected\r\n", selTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		if _, err := c.Select("Old", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		msCmd := c.MultiSearch(
			&imap.SearchSource{Mailboxes: []string{"Archive"}},
			&imap.SearchCriteria{},
			&imap.SearchOptions{ReturnSave: true},
		)
		selCmd := c.Select("New", nil)
		_, _ = msCmd.Wait()
		selData, err := selCmd.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if selData.HighestModSeq == 999 {
			t.Errorf("queued SELECT absorbed pre-completion HIGHESTMODSEQ 999 while MultiSearch with ReturnSave was still pending")
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
