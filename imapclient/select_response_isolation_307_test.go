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

func newSelectIsolation307Peer(t *testing.T, caps string, options *imapclient.Options, serve func(*bufio.Reader, net.Conn) error) (*imapclient.Client, <-chan error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		if _, err := fmt.Fprintf(serverConn, "* PREAUTH [CAPABILITY %s] ready\r\n", caps); err != nil {
			done <- err
			return
		}
		done <- serve(bufio.NewReader(serverConn), serverConn)
	}()
	if options == nil {
		options = &imapclient.Options{}
	}
	options.ResponseTimeout = 2 * time.Second
	c := imapclient.New(clientConn, options)
	t.Cleanup(func() { c.Close(); serverConn.Close() })
	return c, done
}

func readSelectIsolation307Command(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	tag, _, ok := strings.Cut(line, " ")
	if !ok {
		return "", fmt.Errorf("command has no tag: %q", line)
	}
	return tag, nil
}

func writeSelectIsolation307Data(conn net.Conn, tag string, count int, flags, permanent string) error {
	return writeSelectIsolation307DataAccess(conn, tag, count, flags, permanent, "READ-WRITE")
}

func writeSelectIsolation307DataAccess(conn net.Conn, tag string, count int, flags, permanent, access string) error {
	_, err := fmt.Fprintf(conn, "* %d EXISTS\r\n* FLAGS (%s)\r\n* OK [PERMANENTFLAGS (%s)] flags\r\n* OK [UIDVALIDITY 10] validity\r\n* OK [UIDNEXT 4] next\r\n* OK [HIGHESTMODSEQ 3] modseq\r\n%s OK [%s] selected\r\n", count, flags, permanent, tag, access)
	return err
}

func finishSelectIsolation307Noop(br *bufio.Reader, conn net.Conn) error {
	tag, err := readSelectIsolation307Command(br)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(conn, "%s OK noop\r\n", tag)
	return err
}

type selectIsolation307Fetch struct {
	data *imapclient.FetchMessageBuffer
	err  error
}

func TestSameMailboxReselectPreservesUnilateralData(t *testing.T) {
	const body = "Subject: provisional\r\n\r\nbody\r\n"
	for _, readOnly := range []bool{false, true} {
		for _, boundary := range []string{"CLOSED", "omitted", "BAD"} {
			for _, handlers := range []bool{false, true} {
				t.Run(fmt.Sprintf("readOnly=%v/boundary=%s/handlers=%v", readOnly, boundary, handlers), func(t *testing.T) {
					mailboxes := make(chan string, 8)
					fetches := make(chan selectIsolation307Fetch, 2)
					var c *imapclient.Client
					options := &imapclient.Options{}
					if handlers {
						options.UnilateralDataHandler = &imapclient.UnilateralDataHandler{
							Mailbox: func(data *imapclient.UnilateralDataMailbox) {
								mbox := c.Mailbox()
								if mbox == nil || mbox.Name != "INBOX" {
									mailboxes <- "callback lost old mailbox"
									return
								}
								if data.NumMessages != nil {
									mailboxes <- fmt.Sprintf("exists:%d/snapshot:%d", *data.NumMessages, mbox.NumMessages)
								}
								if data.Flags != nil {
									mailboxes <- fmt.Sprintf("flags:%v/snapshot:%v", data.Flags, mbox.Flags)
								}
								if data.PermanentFlags != nil {
									mailboxes <- fmt.Sprintf("permanent:%v/snapshot:%v", data.PermanentFlags, mbox.PermanentFlags)
								}
							},
							Fetch: func(msg *imapclient.FetchMessageData) {
								data, err := msg.Collect()
								fetches <- selectIsolation307Fetch{data, err}
							},
						}
					}
					var done <-chan error
					c, done = newSelectIsolation307Peer(t, "IMAP4rev1 ENABLE QRESYNC CONDSTORE", options, func(br *bufio.Reader, conn net.Conn) error {
						tag, err := readSelectIsolation307Command(br)
						if err != nil {
							return err
						}
						if _, err := fmt.Fprintf(conn, "* ENABLED QRESYNC CONDSTORE\r\n%s OK enabled\r\n", tag); err != nil {
							return err
						}
						tag, err = readSelectIsolation307Command(br)
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
						var update strings.Builder
						update.WriteString("* 2 EXISTS\r\n* FLAGS (\\Seen)\r\n* OK [PERMANENTFLAGS (\\Seen \\*)] flags\r\n* 1 FETCH (UID 1 FLAGS (\\Seen) MODSEQ (2)")
						// More items than the streaming FETCH channel holds ensures
						// provisional messages are consumed before the later boundary.
						for part := 1; part <= 34; part++ {
							fmt.Fprintf(&update, " BODY[%d] \"part-%d\"", part, part)
						}
						fmt.Fprintf(&update, " BODY[] {%d}\r\n%s)\r\n", len(body), body)
						if _, err := fmt.Fprint(conn, update.String()); err != nil {
							return err
						}
						if boundary == "BAD" {
							if _, err := fmt.Fprintf(conn, "%s BAD invalid SELECT\r\n", tag); err != nil {
								return err
							}
						} else {
							if boundary == "CLOSED" {
								if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
									return err
								}
							}
							access := "READ-WRITE"
							if readOnly {
								access = "READ-ONLY"
							}
							if err := writeSelectIsolation307DataAccess(conn, tag, 3, `\Flagged`, `\Flagged`, access); err != nil {
								return err
							}
						}
						return finishSelectIsolation307Noop(br, conn)
					})
					if _, err := c.Enable(imap.CapQResync).Wait(); err != nil {
						t.Fatal(err)
					}
					if _, err := c.Select("INBOX", nil).Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := c.Select("inbox", &imap.SelectOptions{ReadOnly: readOnly, QResync: &imap.QResyncData{UIDValidity: 10, ModSeq: 1}}).Wait()
					if (err != nil) != (boundary == "BAD") {
						t.Fatalf("SELECT error = %v, boundary %s", err, boundary)
					}
					if err := c.Noop().Wait(); err != nil {
						t.Fatalf("NOOP after FETCH literal and SELECT: %v", err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if boundary == "BAD" {
						mbox := c.Mailbox()
						if mbox == nil || mbox.NumMessages != 2 || !reflect.DeepEqual(mbox.Flags, []imap.Flag{imap.FlagSeen}) || !reflect.DeepEqual(mbox.PermanentFlags, []imap.Flag{imap.FlagSeen, imap.FlagWildcard}) {
							t.Errorf("BAD lost old mailbox updates: %+v", mbox)
						}
					} else {
						if data.NumMessages != 3 || !reflect.DeepEqual(data.Flags, []imap.Flag{imap.FlagFlagged}) || !reflect.DeepEqual(data.PermanentFlags, []imap.Flag{imap.FlagFlagged}) {
							t.Errorf("opening data changed: %+v", data)
						}
						wantModified := 0
						if boundary == "omitted" {
							wantModified = 1
						}
						if len(data.Modified) != wantModified {
							t.Errorf("Modified count=%d, want %d", len(data.Modified), wantModified)
						}
					}
					if handlers && boundary != "omitted" {
						want := []string{`exists:2/snapshot:2`, `flags:[\Seen]/snapshot:[\Seen]`, `permanent:[\Seen \*]/snapshot:[\Seen \*]`}
						for _, expected := range want {
							select {
							case got := <-mailboxes:
								if got != expected {
									t.Errorf("mailbox callback=%q, want %q", got, expected)
								}
							case <-time.After(300 * time.Millisecond):
								t.Errorf("missing mailbox callback %q", expected)
							}
						}
						select {
						case got := <-fetches:
							if got.err != nil || got.data.UID != 1 || got.data.ModSeq != 2 || len(got.data.BodySection) != 35 || string(got.data.BodySection[34].Bytes) != body {
								t.Errorf("FETCH callback lost data: %+v, %v", got.data, got.err)
							}
						case <-time.After(300 * time.Millisecond):
							t.Error("missing FETCH callback")
						}
					} else {
						if len(mailboxes) != 0 || len(fetches) != 0 {
							t.Errorf("unexpected unsolicited callbacks: mailbox=%d FETCH=%d", len(mailboxes), len(fetches))
						}
					}
				})
			}
		}
	}
}

func TestReselectUnilateralCallbackMayCloseClient(t *testing.T) {
	closed := make(chan error, 1)
	var c *imapclient.Client
	c, _ = newSelectIsolation307Peer(t, "IMAP4rev2 CONDSTORE", &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil && *data.NumMessages == 2 {
				closed <- c.Close()
			}
		},
	}}, func(br *bufio.Reader, conn net.Conn) error {
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
		if _, err := fmt.Fprint(conn, "* 2 EXISTS\r\n* OK [CLOSED] old selection ended\r\n"); err != nil {
			return err
		}
		return writeSelectIsolation307Data(conn, tag, 3, `\Seen`, `\Seen`)
	})
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	selection := c.Select("INBOX", nil)
	completed := make(chan error, 1)
	go func() { _, err := selection.Wait(); completed <- err }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("provisional unilateral callback was lost or Close deadlocked")
	}
	select {
	case err := <-completed:
		if err == nil {
			t.Error("SELECT succeeded after the callback should have closed the client")
		}
	case <-time.After(time.Second):
		t.Error("closing from callback did not release pending SELECT")
	}
}
