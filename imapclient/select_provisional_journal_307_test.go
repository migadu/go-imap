package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

const journal307Marker = "* OK provisional-journal-ready\r\n"

// Reading one byte at a time makes the marker an acknowledgement that the
// decoder consumed the preceding complete FETCH responses, rather than merely
// that a buffered transport accepted their bytes.
type journal307ObservedConn struct {
	net.Conn
	seen   chan struct{}
	last   string
	closed bool
}

func (c *journal307ObservedConn) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := c.Conn.Read(p)
	if n > 0 && !c.closed {
		c.last += string(p[:n])
		if len(c.last) > len(journal307Marker) {
			c.last = c.last[len(c.last)-len(journal307Marker):]
		}
		if c.last == journal307Marker {
			c.closed = true
			close(c.seen)
		}
	}
	return n, err
}

func newJournal307Peer(t *testing.T, options *imapclient.Options, observe bool, serve func(*bufio.Reader, net.Conn) error) (*imapclient.Client, <-chan struct{}) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	if err := serverConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{})
	var conn net.Conn = clientConn
	if observe {
		conn = &journal307ObservedConn{Conn: clientConn, seen: seen}
	}
	go func() {
		if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ENABLE CONDSTORE QRESYNC] ready\r\n"); err != nil {
			return
		}
		_ = serve(bufio.NewReader(serverConn), serverConn)
	}()
	options.ResponseTimeout = 10 * time.Second
	options.LiteralReadTimeout = 10 * time.Second
	c := imapclient.New(conn, options)
	t.Cleanup(func() { clientConn.Close(); serverConn.Close(); c.Close() })
	return c, seen
}

func journal307Opening(br *bufio.Reader, conn net.Conn, qresync bool, count int) error {
	if qresync {
		tag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "* ENABLED QRESYNC CONDSTORE\r\n%s OK enabled\r\n", tag); err != nil {
			return err
		}
	}
	tag, err := readSelectIsolation307Command(br)
	if err != nil {
		return err
	}
	return writeJournal307Data(conn, tag, count, `\Answered`, `\Answered`)
}

func writeJournal307Data(conn net.Conn, tag string, count int, flags, permanent string) error {
	_, err := fmt.Fprintf(conn, "* %d EXISTS\r\n* FLAGS (%s)\r\n* OK [PERMANENTFLAGS (%s)] flags\r\n* OK [UIDVALIDITY 10] validity\r\n* OK [UIDNEXT %d] next\r\n* OK [HIGHESTMODSEQ 100] modseq\r\n%s OK [READ-WRITE] selected\r\n", count, flags, permanent, count+1, tag)
	return err
}

func journal307StartSelection(t *testing.T, c *imapclient.Client, qresync bool) {
	t.Helper()
	if qresync {
		if _, err := c.Enable(imap.CapQResync).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Select("Work", nil).Wait(); err != nil {
		t.Fatal(err)
	}
}

func journal307WaitError(t *testing.T, wait func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("pending operation succeeded after the connection should have failed")
		}
	case <-time.After(2 * time.Second):
		t.Error("pending operation did not complete promptly")
	}
}

func TestProvisionalSelectJournalBudgetDoesNotWaitForItsOwnBoundary(t *testing.T) {
	for _, kind := range []string{"literals", "metadata"} {
		for _, budget := range []int64{4096, 1 << 20} {
			t.Run(fmt.Sprintf("%s/budget=%d", kind, budget), func(t *testing.T) {
				c, _ := newJournal307Peer(t, &imapclient.Options{
					MaxFetchBufferSize: budget,
					MaxLiteralSize:     2048,
					UnilateralDataHandler: &imapclient.UnilateralDataHandler{Fetch: func(msg *imapclient.FetchMessageData) {
						_, _ = msg.Collect()
					}},
				}, false, func(br *bufio.Reader, conn net.Conn) error {
					if err := journal307Opening(br, conn, true, 50); err != nil {
						return err
					}
					tag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					noop, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					if kind == "literals" {
						body := strings.Repeat("b", 1024)
						for n := 1; n <= 5; n++ {
							if _, err := fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (\\Seen) MODSEQ (%d) BODY[] {1024}\r\n%s)\r\n", n, n, n+1, body); err != nil {
								return err
							}
						}
					} else {
						for n := 1; n <= 32; n++ {
							if _, err := fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (%s) MODSEQ (%d))\r\n", n, n, strings.Repeat("x", 200), n+1); err != nil {
								return err
							}
						}
					}
					if budget == 4096 {
						// There is no boundary for a consumer to reach and free the
						// journal. Exceeding its cap must fail, rather than wait.
						return nil
					}
					if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
						return err
					}
					if err := writeJournal307Data(conn, tag, 50, `\Flagged`, `\Flagged`); err != nil {
						return err
					}
					_, err = fmt.Fprintf(conn, "%s OK noop\r\n", noop)
					return err
				})
				journal307StartSelection(t, c, true)
				selection := c.Select("Work", &imap.SelectOptions{QResync: &imap.QResyncData{UIDValidity: 10, ModSeq: 1}})
				noop := c.Noop()
				if budget == 4096 {
					journal307WaitError(t, func() error { _, err := selection.Wait(); return err })
					journal307WaitError(t, noop.Wait)
				} else {
					if _, err := selection.Wait(); err != nil {
						t.Fatalf("valid responses with room in journal: %v", err)
					}
					if err := noop.Wait(); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestProvisionalSelectJournalCancellationAndEOFReleaseCommands(t *testing.T) {
	for _, ending := range []string{"cancel", "EOF"} {
		t.Run(ending, func(t *testing.T) {
			terminate := make(chan struct{})
			var once sync.Once
			end := func() { once.Do(func() { close(terminate) }) }
			t.Cleanup(end)
			c, seen := newJournal307Peer(t, &imapclient.Options{MaxFetchBufferSize: 1 << 20, UnilateralDataHandler: &imapclient.UnilateralDataHandler{Fetch: func(msg *imapclient.FetchMessageData) {
				_, _ = msg.Collect()
			}}}, true, func(br *bufio.Reader, conn net.Conn) error {
				if err := journal307Opening(br, conn, true, 3); err != nil {
					return err
				}
				if _, err := readSelectIsolation307Command(br); err != nil {
					return err
				}
				if _, err := readSelectIsolation307Command(br); err != nil {
					return err
				}
				for n := 1; n <= 2; n++ {
					if _, err := fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (\\Seen) MODSEQ (2) BODY[] {4}\r\nbody)\r\n", n, n); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprint(conn, journal307Marker); err != nil {
					return err
				}
				<-terminate
				return conn.Close()
			})
			journal307StartSelection(t, c, true)
			selection := c.Select("Work", &imap.SelectOptions{QResync: &imap.QResyncData{UIDValidity: 10, ModSeq: 1}})
			noop := c.Noop()
			select {
			case <-seen:
			case <-time.After(2 * time.Second):
				t.Fatal("decoder did not consume provisional responses before cancellation")
			}
			if ending == "cancel" {
				closed := make(chan struct{})
				go func() { _ = c.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(2 * time.Second):
					t.Error("Close did not release the provisional journal")
				}
			}
			end()
			journal307WaitError(t, func() error { _, err := selection.Wait(); return err })
			journal307WaitError(t, noop.Wait)
		})
	}
}

func TestProvisionalSelectJournalPreservesMixedEventOrder(t *testing.T) {
	// EXISTS and the available/permanent flags may be opening SELECT data.
	// Removals, non-QRESYNC FETCH and OLDNAME are definite old-mailbox events.
	// When CLOSED is omitted on success, deliver only the definite events on
	// the old snapshot before publishing the new selection.
	for _, removal := range []string{"EXPUNGE", "VANISHED"} {
		for _, boundary := range []string{"CLOSED", "BAD", "omitted"} {
			t.Run(removal+"/"+boundary, func(t *testing.T) {
				events := make(chan string, 16)
				var c *imapclient.Client
				snapshot := func() string {
					m := c.Mailbox()
					if m == nil {
						return "nil mailbox"
					}
					return fmt.Sprintf("count=%d flags=%v permanent=%v", m.NumMessages, m.Flags, m.PermanentFlags)
				}
				options := &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Mailbox: func(data *imapclient.UnilateralDataMailbox) {
						if data.NumMessages != nil {
							events <- "EXISTS " + snapshot()
						} else if data.Flags != nil {
							events <- "FLAGS " + snapshot()
						} else if data.PermanentFlags != nil {
							events <- "PERMANENTFLAGS " + snapshot()
						}
					},
					Fetch: func(msg *imapclient.FetchMessageData) {
						data, err := msg.Collect()
						if err != nil || data.UID != 1 || len(data.BodySection) != 1 || string(data.BodySection[0].Bytes) != "body" {
							events <- fmt.Sprintf("invalid FETCH: %+v %v", data, err)
							return
						}
						events <- "FETCH " + snapshot()
					},
					Expunge:  func(uint32) { events <- "EXPUNGE " + snapshot() },
					Vanished: func(*imap.VanishedData) { events <- "VANISHED " + snapshot() },
					List:     func(*imap.ListData) { events <- "OLDNAME " + snapshot() },
				}}
				qresync := removal == "VANISHED"
				var done <-chan error
				c, done = newSelectIsolation307Peer(t, "IMAP4rev1 ENABLE CONDSTORE QRESYNC", options, func(br *bufio.Reader, conn net.Conn) error {
					if err := journal307Opening(br, conn, qresync, 3); err != nil {
						return err
					}
					tag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					if _, err := fmt.Fprint(conn, "* 5 EXISTS\r\n* FLAGS ()\r\n"); err != nil {
						return err
					}
					if !qresync {
						if _, err := fmt.Fprint(conn, "* 1 FETCH (UID 1 FLAGS (\\Seen) MODSEQ (2) BODY[] {4}\r\nbody)\r\n* 2 EXPUNGE\r\n"); err != nil {
							return err
						}
					} else if _, err := fmt.Fprint(conn, "* VANISHED 4:5\r\n"); err != nil {
						return err
					}
					if _, err := fmt.Fprint(conn, "* OK [PERMANENTFLAGS ()] flags\r\n* LIST () \"/\" Work (\"OLDNAME\" (\"Drafts\"))\r\n"); err != nil {
						return err
					}
					if boundary == "BAD" {
						_, err = fmt.Fprintf(conn, "%s BAD invalid selection\r\n", tag)
					} else {
						if boundary == "CLOSED" {
							if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
								return err
							}
						}
						err = writeJournal307Data(conn, tag, 7, `\Flagged`, `\Flagged`)
					}
					if err != nil {
						return err
					}
					return finishSelectIsolation307Noop(br, conn)
				})
				journal307StartSelection(t, c, qresync)
				selectOptions := &imap.SelectOptions{CondStore: true}
				if qresync {
					selectOptions = &imap.SelectOptions{QResync: &imap.QResyncData{UIDValidity: 10, ModSeq: 1}}
				}
				_, err := c.Select("Work", selectOptions).Wait()
				if (err != nil) != (boundary == "BAD") {
					t.Fatalf("selection error=%v", err)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				count := 4
				if qresync {
					count = 3
				}
				var want []string
				if boundary != "omitted" {
					want = append(want, `EXISTS count=5 flags=[\Answered] permanent=[\Answered]`, `FLAGS count=5 flags=[] permanent=[\Answered]`)
					if !qresync {
						want = append(want, `FETCH count=5 flags=[] permanent=[\Answered]`)
					}
					want = append(want, fmt.Sprintf(`%s count=%d flags=[] permanent=[\Answered]`, removal, count), fmt.Sprintf(`PERMANENTFLAGS count=%d flags=[] permanent=[]`, count), fmt.Sprintf(`OLDNAME count=%d flags=[] permanent=[]`, count))
				} else {
					count = 2
					if qresync {
						count = 1
					} else {
						want = append(want, `FETCH count=3 flags=[\Answered] permanent=[\Answered]`)
					}
					want = append(want, fmt.Sprintf(`%s count=%d flags=[\Answered] permanent=[\Answered]`, removal, count), fmt.Sprintf(`OLDNAME count=%d flags=[\Answered] permanent=[\Answered]`, count))
				}
				var got []string
				for range want {
					select {
					case event := <-events:
						got = append(got, event)
					case <-time.After(300 * time.Millisecond):
						got = append(got, "missing event")
					}
				}
				if !reflect.DeepEqual(got, want) || len(events) != 0 {
					t.Errorf("callback order/snapshots:\n got %q (+%d extra)\nwant %q", got, len(events), want)
				}
			})
		}
	}
}
