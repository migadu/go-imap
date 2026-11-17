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

// SEARCHRES is an opaque UID set. It still needs the ordinary UID-late
// buffering, duplicate suppression and unsolicited-response lifetime rules.
func TestFetchSearchResUIDLateAndUnsolicited(t *testing.T) {
	unsolicited := make(chan selectIsolation307Fetch, 4)
	c := newUIDBufferLimitClient(t, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Fetch: func(msg *imapclient.FetchMessageData) {
				data, err := msg.Collect()
				unsolicited <- selectIsolation307Fetch{data, err}
			},
		},
	}, func(conn net.Conn, br *bufio.Reader) error {
		tag, err := readFetchSearchResWireCommand(br)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn,
			"* 8 FETCH (FLAGS (\\Answered))\r\n"+
				"* 7 FETCH (BODY[] {4}\r\ndata UID 42)\r\n"+
				"* 7 FETCH (UID 42 FLAGS (\\Seen))\r\n"+
				"%s OK fetched\r\n"+
				"* 9 FETCH (UID 99 FLAGS (\\Flagged))\r\n", tag); err != nil {
			return err
		}
		return finishSelectIsolation307Noop(br, conn)
	})
	section := &imap.FetchItemBodySection{}
	messages, err := c.Fetch(imap.SearchRes(), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].UID != 42 || messages[0].SeqNum != 7 || string(messages[0].FindBodySection(section)) != "data" {
		t.Errorf("FETCH $ returned %+v, want one message with sequence 7, UID 42 and body data", messages)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint32]imap.UID)
	for i := 0; i < 3; i++ {
		select {
		case result := <-unsolicited:
			if result.err != nil {
				t.Fatal(result.err)
			}
			seen[result.data.SeqNum] = result.data.UID
		case <-time.After(2 * time.Second):
			t.Fatal("missing UID-less, duplicate or post-completion unsolicited response")
		}
	}
	if len(seen) != 3 || seen[8] != 0 || seen[7] != 42 || seen[9] != 99 {
		t.Fatalf("unsolicited sequence/UID pairs = %v, want 8/0, 7/42 and 9/99", seen)
	}
}

// A numeric command sent first retains its matching response. The opaque
// marker then owns previously unseen UIDs until its tagged completion; later
// responses can belong to another numeric command.
func TestFetchSearchResPendingCommandOwnership(t *testing.T) {
	c := newUIDBufferLimitClient(t, nil, func(conn net.Conn, br *bufio.Reader) error {
		first, err := readUIDFetchWireCommand(br)
		if err != nil {
			return err
		}
		saved, err := readFetchSearchResWireCommand(br)
		if err != nil {
			return err
		}
		last, err := readUIDFetchWireCommand(br)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn,
			"* 2 FETCH (FLAGS (\\Flagged) UID 42)\r\n"+
				"* 1 FETCH (FLAGS (\\Seen) UID 41)\r\n"+
				"%s OK first\r\n%s OK saved\r\n"+
				"* 3 FETCH (FLAGS (\\Answered) UID 43)\r\n%s OK last\r\n", first, saved, last)
		return err
	})
	first := c.Fetch(imap.UIDSetNum(41), &imap.FetchOptions{Flags: true})
	saved := c.Fetch(imap.SearchRes(), &imap.FetchOptions{Flags: true})
	last := c.Fetch(imap.UIDSetNum(43), &imap.FetchOptions{Flags: true})
	for _, tc := range []struct {
		name string
		cmd  *imapclient.FetchCommand
		uid  imap.UID
		flag imap.Flag
	}{
		{"first numeric", first, 41, imap.FlagSeen},
		{"saved", saved, 42, imap.FlagFlagged},
		{"last numeric", last, 43, imap.FlagAnswered},
	} {
		messages, err := tc.cmd.Collect()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(messages) != 1 || messages[0].UID != tc.uid || !slices.Equal(messages[0].Flags, []imap.Flag{tc.flag}) {
			t.Errorf("%s returned %+v, want UID %d with %s", tc.name, messages, tc.uid, tc.flag)
		}
	}
}

func TestFetchSearchResSelectionAndAuthBarriers(t *testing.T) {
	for _, barrier := range []string{"SELECT", "UNSELECT", "CLOSE", "UNAUTHENTICATE"} {
		t.Run(barrier, func(t *testing.T) {
			c, done := newSelectIsolation307Peer(t, "IMAP4rev2 UNAUTHENTICATE", nil, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err := writeSelectIsolation307Data(conn, tag, 3, `\Seen`, `\Seen`); err != nil {
					return err
				}
				barrierTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				fetchTag, err := readFetchSearchResWireCommand(br)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprint(conn, "* 1 FETCH (UID 99 FLAGS (\\Answered))\r\n"); err != nil {
					return err
				}
				if barrier == "SELECT" {
					if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
						return err
					}
					if err := writeSelectIsolation307Data(conn, barrierTag, 2, `\Flagged`, `\Flagged`); err != nil {
						return err
					}
				} else if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY IMAP4rev2 UNAUTHENTICATE] completed\r\n", barrierTag); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "* 2 FETCH (UID 42 FLAGS (\\Flagged))\r\n%s OK fetched\r\n", fetchTag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			if _, err := c.Select("Work", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			var waitBarrier func() error
			switch barrier {
			case "SELECT":
				cmd := c.Select("Other", nil)
				waitBarrier = func() error { _, err := cmd.Wait(); return err }
			case "UNSELECT":
				waitBarrier = c.Unselect().Wait
			case "CLOSE":
				waitBarrier = c.UnselectAndExpunge().Wait
			case "UNAUTHENTICATE":
				waitBarrier = c.Unauthenticate().Wait
			}
			fetch := c.Fetch(imap.SearchRes(), &imap.FetchOptions{Flags: true})
			if err := waitBarrier(); err != nil {
				t.Fatal(err)
			}
			messages, err := fetch.Collect()
			if err != nil {
				t.Fatal(err)
			}
			if barrier == "SELECT" {
				if len(messages) != 1 || messages[0].UID != 42 {
					t.Errorf("FETCH $ crossed SELECT boundary or lost later result: %+v", messages)
				}
			} else if len(messages) != 0 {
				t.Errorf("FETCH $ claimed data across %s: %+v", barrier, messages)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFetchSearchResWithoutSelectedMailbox(t *testing.T) {
	c, done := newSelectIsolation307Peer(t, "IMAP4rev2", nil, func(br *bufio.Reader, conn net.Conn) error {
		tag, err := readFetchSearchResWireCommand(br)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "* 1 FETCH (UID 42 FLAGS (\\Seen))\r\n%s OK fetched\r\n", tag); err != nil {
			return err
		}
		return finishSelectIsolation307Noop(br, conn)
	})
	messages, err := c.Fetch(imap.SearchRes(), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil || len(messages) != 0 {
		t.Fatalf("FETCH $ without a selection = %+v, %v; want empty", messages, err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func readFetchSearchResWireCommand(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	tag, command, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(command, "UID FETCH $ (") {
		return "", fmt.Errorf("expected UID FETCH $, got %q", line)
	}
	return tag, nil
}
