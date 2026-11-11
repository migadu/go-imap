package imapclient

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

type searchCapsCommandCase struct {
	name, prefix string
	search       bool
	send         func(*Client) error
}

func searchCapsCommandCases() []searchCapsCommandCase {
	criteria := &imap.SearchCriteria{Body: []string{"café"}, ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 5}}
	sortOptions := &SortOptions{SortCriteria: []imap.SortCriterion{{Key: imap.SortKeyArrival}}, SearchCriteria: criteria}
	threadOptions := &ThreadOptions{Algorithm: imap.ThreadReferences, SearchCriteria: criteria}
	return []searchCapsCommandCase{
		{"SEARCH", "SEARCH ", true, func(c *Client) error { _, err := c.Search(criteria, nil).Wait(); return err }},
		{"UID SEARCH", "UID SEARCH ", true, func(c *Client) error { _, err := c.UIDSearch(criteria, nil).Wait(); return err }},
		{"ESEARCH", "ESEARCH ", true, func(c *Client) error { _, err := c.MultiSearch(nil, criteria, nil).Wait(); return err }},
		{"SORT", "SORT (ARRIVAL) UTF-8 ", false, func(c *Client) error { _, err := c.Sort(sortOptions).Wait(); return err }},
		{"UID SORT", "UID SORT (ARRIVAL) UTF-8 ", false, func(c *Client) error { _, err := c.UIDSort(sortOptions).Wait(); return err }},
		{"THREAD", "THREAD REFERENCES UTF-8 ", false, func(c *Client) error { _, err := c.Thread(threadOptions).Wait(); return err }},
		{"UID THREAD", "UID THREAD REFERENCES UTF-8 ", false, func(c *Client) error { _, err := c.UIDThread(threadOptions).Wait(); return err }},
	}
}

// Set up a real TLS, authenticated, selected session while CAPABILITY requests
// are refused. The next request, initiated by the search constructor itself,
// learns the capabilities supplied by the test. No scheduler ordering is used.
func newSearchCapsTLSClient(t *testing.T, cert tls.Certificate, caps string, hold bool) (*Client, <-chan string, <-chan struct{}) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	commands := make(chan string, 1)
	requested := make(chan struct{})
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n")
		br := bufio.NewReader(serverConn)
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		io.WriteString(serverConn, strings.Fields(line)[0]+" OK begin TLS\r\n")
		conn := tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{cert}})
		br = bufio.NewReader(conn)
		selected := false
		var once sync.Once
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, command, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
			switch {
			case command == "CAPABILITY" && !selected:
				io.WriteString(conn, tag+" NO unavailable for now\r\n")
			case command == "CAPABILITY":
				once.Do(func() { close(requested) })
				if hold {
					io.Copy(io.Discard, br)
					return
				}
				io.WriteString(conn, "* CAPABILITY "+caps+"\r\n"+tag+" OK caps\r\n")
			case strings.HasPrefix(command, "LOGIN "):
				io.WriteString(conn, tag+" OK authenticated\r\n")
			case strings.HasPrefix(command, "SELECT "):
				selected = true
				io.WriteString(conn, "* 1 EXISTS\r\n"+tag+" OK selected\r\n")
			default:
				// Preserve both the literal header and payload in the captured
				// command, so the test distinguishes literals from quoted UTF-8.
				if i := strings.LastIndex(command, "{"); i >= 0 && strings.HasSuffix(command, "+}") {
					n, err := strconv.Atoi(command[i+1 : len(command)-2])
					if err != nil {
						t.Error(err)
						return
					}
					literal := make([]byte, n)
					if _, err := io.ReadFull(br, literal); err != nil {
						return
					}
					tail, err := br.ReadString('\n')
					if err != nil {
						return
					}
					command += "<" + string(literal) + ">" + strings.TrimSuffix(tail, "\r\n")
				}
				commands <- command
				io.WriteString(conn, tag+" OK searched\r\n")
			}
		}
	}()
	client := New(clientConn, &Options{ResponseTimeout: 5 * time.Second})
	t.Cleanup(func() { client.Close(); serverConn.Close() })
	if err := client.startTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatal(err)
	}
	// Every refresh triggered by TLS or LOGIN has finished unsuccessfully.
	// SELECT itself does not need capabilities and can establish valid state.
	awaitCancellationCondition(t, "refused post-TLS capabilities", func() bool {
		client.mutex.Lock()
		defer client.mutex.Unlock()
		if client.caps != nil || client.capsRefresh != nil {
			return false
		}
		for _, cmd := range client.pendingCmds {
			if _, ok := cmd.(*CapabilityCommand); ok {
				return false
			}
		}
		return true
	})
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	return client, commands, requested
}

func TestSearchCommandsLearnPostTLSCharsetAndCapabilities(t *testing.T) {
	cert := startTLSRefreshCertificate(t)
	for _, mode := range []struct {
		name, caps        string
		quoted, condStore bool
	}{
		{"rev1", "IMAP4rev1 LITERAL+ CONDSTORE ESEARCH SORT THREAD=REFERENCES MULTISEARCH", false, true},
		{"rev2", "IMAP4rev2 CONDSTORE ESEARCH SORT THREAD=REFERENCES MULTISEARCH", true, true},
		{"without CONDSTORE", "IMAP4rev1 LITERAL+ ESEARCH SORT THREAD=REFERENCES MULTISEARCH", false, false},
	} {
		for _, tc := range searchCapsCommandCases() {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				client, commands, _ := newSearchCapsTLSClient(t, cert, mode.caps, false)
				done := make(chan error, 1)
				go func() { done <- tc.send(client) }()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("search constructor blocked with post-TLS capabilities unknown")
				}
				wire := <-commands
				prefix := tc.prefix
				if tc.search && !mode.quoted {
					prefix += "CHARSET UTF-8 "
				}
				if !strings.HasPrefix(wire, prefix) || (tc.search && mode.quoted && strings.Contains(wire, "CHARSET")) {
					t.Errorf("wire charset = %q, want prefix %q", wire, prefix)
				}
				body := `BODY "café"`
				if !mode.quoted {
					body = "BODY {5+}<café>"
				}
				if !strings.Contains(wire, body) || strings.Contains(wire, "MODSEQ 5") != mode.condStore {
					t.Errorf("wire criteria = %q, want body %q and CONDSTORE %v", wire, body, mode.condStore)
				}
			})
		}
	}
}

func TestSearchCommandsCapabilityWaitIsCancellableAfterTLS(t *testing.T) {
	cert := startTLSRefreshCertificate(t)
	for _, tc := range searchCapsCommandCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, commands, requested := newSearchCapsTLSClient(t, cert, "", true)
			done := make(chan error, 1)
			go func() { done <- tc.send(client) }()
			select {
			case <-requested:
			case <-time.After(3 * time.Second):
				t.Fatal("constructor did not request its missing capabilities")
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("constructor cancellation = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("constructor did not observe cancellation")
			}
			if len(commands) != 0 {
				t.Error("search command was sent after cancellation")
			}
		})
	}
}
