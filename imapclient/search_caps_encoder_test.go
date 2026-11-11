package imapclient

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

// SEARCH, ESEARCH, SORT and THREAD encode MODSEQ only for a server with
// CONDSTORE, so they need the capabilities. Asked for once the command had
// the encoder, with none known, Caps waited for a CAPABILITY command that
// needed that encoder too: the command stalled until the response timeout.
// Here the greeting carries no capabilities and the server refuses the
// CAPABILITY sent for it, leaving them unknown. Issue #178.
func TestSearchCommandsLearnCapabilitiesFirst(t *testing.T) {
	criteria := &imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 5}}
	for _, tc := range []struct {
		name string
		send func(c *Client) error
		want string
	}{
		{"SEARCH", func(c *Client) error { _, err := c.Search(criteria, nil).Wait(); return err }, "SEARCH MODSEQ 5"},
		{"UID SEARCH", func(c *Client) error { _, err := c.UIDSearch(criteria, nil).Wait(); return err }, "UID SEARCH MODSEQ 5"},
		{"ESEARCH", func(c *Client) error { _, err := c.MultiSearch(nil, criteria, nil).Wait(); return err }, "ESEARCH MODSEQ 5"},
		{"SORT", func(c *Client) error {
			_, err := c.Sort(&SortOptions{SortCriteria: []imap.SortCriterion{{Key: imap.SortKeyArrival}}, SearchCriteria: criteria}).Wait()
			return err
		}, "SORT (ARRIVAL) UTF-8 MODSEQ 5"},
		{"THREAD", func(c *Client) error {
			_, err := c.Thread(&ThreadOptions{Algorithm: imap.ThreadReferences, SearchCriteria: criteria}).Wait()
			return err
		}, "THREAD REFERENCES UTF-8 MODSEQ 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()

			refused := make(chan struct{})
			commands := make(chan string, 4)
			go func() {
				io.WriteString(serverConn, "* PREAUTH ready\r\n")
				br := bufio.NewReader(serverConn)
				first := true
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					tag, cmd, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
					switch {
					case cmd == "CAPABILITY" && first:
						first = false
						io.WriteString(serverConn, tag+" NO not now\r\n")
						close(refused)
					case cmd == "CAPABILITY":
						io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 CONDSTORE ESEARCH SORT THREAD=REFERENCES MULTISEARCH\r\n"+tag+" OK done\r\n")
					default:
						commands <- cmd
						io.WriteString(serverConn, tag+" OK done\r\n")
					}
				}
			}()

			c := New(clientConn, &Options{ResponseTimeout: 10 * time.Second})
			defer c.Close()
			select {
			case <-refused:
			case <-time.After(2 * time.Second):
				t.Fatal("the client did not ask for the capabilities the greeting left out")
			}
			// Wait for the refused request to finish, the capabilities unknown.
			for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
				c.mutex.Lock()
				settled := c.capsRefresh == nil && c.caps == nil
				c.mutex.Unlock()
				if settled {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the refused CAPABILITY never finished")
				}
			}

			done := make(chan error, 1)
			go func() { done <- tc.send(c) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%v = %v", tc.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%v still blocked after 2s, with the capabilities unknown", tc.name)
			}
			if got := <-commands; got != tc.want {
				t.Errorf("sent %q, want %q", got, tc.want)
			}
		})
	}
}

// The same with a CAPABILITY command queued behind the search command, as
// after STARTTLS or a LOGIN whose OK carries no capabilities: the search
// command took the encoder first, then waited for the queued command, which
// waited for the encoder. Issue #178.
func TestSearchCommandsAheadOfQueuedCapabilities(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	commands := make(chan string, 4)
	go func() {
		io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] ready\r\n")
		br := bufio.NewReader(serverConn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, cmd, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
			if cmd == "CAPABILITY" {
				io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 CONDSTORE\r\n")
			} else {
				commands <- cmd
			}
			io.WriteString(serverConn, tag+" OK done\r\n")
		}
	}()

	c := New(clientConn, &Options{ResponseTimeout: 10 * time.Second})
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	// The search command waits for the encoder first, the CAPABILITY command
	// sent for the reset capabilities second.
	c.encMutex.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := c.UIDSearch(&imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 5}}, nil).Wait()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.resetCaps(nil)
	time.Sleep(50 * time.Millisecond)
	c.encMutex.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UIDSearch() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UIDSearch() still blocked after 2s, ahead of a queued CAPABILITY")
	}
	if got, want := <-commands, "UID SEARCH MODSEQ 5"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}
