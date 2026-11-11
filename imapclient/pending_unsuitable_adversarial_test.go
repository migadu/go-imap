package imapclient

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

func TestPendingSelectOnlyPermitsAuthenticatedTransition(t *testing.T) {
	for _, state := range []imap.ConnState{imap.ConnStateNone, imap.ConnStateNotAuthenticated, imap.ConnStateAuthenticated, imap.ConnStateSelected, imap.ConnStateLogout} {
		client := &Client{state: state, pendingCmds: []command{&SelectCommand{}}}
		wantIgnored := state != imap.ConnStateAuthenticated && state != imap.ConnStateSelected
		if ignored := client.ignoresMailboxData(); ignored != wantIgnored {
			t.Errorf("mailbox data in %v with SELECT pending: ignored = %v, want %v", state, ignored, wantIgnored)
		}
	}
}

// SELECT is only a valid transition out of authenticated state. Merely
// queueing it before authentication must not authorize earlier pending
// commands to receive injected mailbox data.
func TestUnauthenticatedPendingSelectDoesNotAuthorizeMailboxData(t *testing.T) {
	for _, expunge := range []bool{false, true} {
		name := "FETCH"
		if expunge {
			name = "EXPUNGE"
		}
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			go func() {
				io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
				br := bufio.NewReader(serverConn)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				firstTag := strings.Fields(line)[0]
				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				selectTag := strings.Fields(line)[0]
				data := "* 1 FETCH (UID 42 BODY[] {5}\r\nhello)\r\n"
				if expunge {
					data = "* 2 EXPUNGE\r\n"
				}
				data = "* FLAGS (\\Seen)\r\n* 3 EXISTS\r\n* VANISHED 43\r\n* OK [PERMANENTFLAGS (\\Seen)] flags\r\n" + data
				io.WriteString(serverConn, data+firstTag+" OK completed\r\n"+selectTag+" NO authenticate first\r\n")
				if line, err = br.ReadString('\n'); err == nil {
					io.WriteString(serverConn, strings.Fields(line)[0]+" OK noop\r\n")
				}
			}()
			var callbacks atomic.Int32
			client := New(clientConn, &Options{
				ResponseTimeout: time.Second,
				UnilateralDataHandler: &UnilateralDataHandler{
					Mailbox:  func(*UnilateralDataMailbox) { callbacks.Add(1) },
					Vanished: func(*imap.VanishedData) { callbacks.Add(1) },
					Fetch:    func(*FetchMessageData) { callbacks.Add(1) },
					Expunge:  func(uint32) { callbacks.Add(1) },
				},
			})
			defer client.Close()
			var collect func() (int, error)
			if expunge {
				cmd := client.Expunge()
				collect = func() (int, error) { result, err := cmd.Collect(); return len(result), err }
			} else {
				cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{}}})
				collect = func() (int, error) { result, err := cmd.Collect(); return len(result), err }
			}
			selection := client.Select("INBOX", nil)
			if count, err := collect(); err != nil || count != 0 {
				t.Errorf("%s with unauthenticated SELECT pending returned %d results, %v; want none", name, count, err)
			}
			data, err := selection.Wait()
			if err == nil {
				t.Error("SELECT should have failed before authentication")
			}
			if data.NumMessages != 0 || len(data.Flags) != 0 || len(data.PermanentFlags) != 0 || callbacks.Load() != 0 {
				t.Errorf("unauthenticated selection accepted mailbox data: %+v, %d callbacks", data, callbacks.Load())
			}
			if err := client.Noop().Wait(); err != nil {
				t.Errorf("NOOP after ignored data: %v", err)
			}
		})
	}
}

// Ignored UID data is drained as a stream even when the literal precedes UID.
// It must neither consume the routing budget nor stall cancellation.
func TestIgnoredUIDFetchLiteralBudgetAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "drain"
		if cancel {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			offered := make(chan struct{})
			go func() {
				io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 BINARY] ready\r\n")
				br := bufio.NewReader(serverConn)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				tag := strings.Fields(line)[0]
				if cancel {
					io.WriteString(serverConn, "* 1 FETCH (BINARY[] ~{32768}\r\nabc")
					close(offered)
					io.Copy(io.Discard, br)
					return
				}
				io.WriteString(serverConn, "* 1 FETCH (BINARY[] ~{32768}\r\n"+strings.Repeat("\x00", 32768)+" UID 42)\r\n"+tag+" OK fetch\r\n")
				if line, err = br.ReadString('\n'); err == nil {
					io.WriteString(serverConn, strings.Fields(line)[0]+" OK noop\r\n")
				}
			}()
			client := New(clientConn, &Options{ResponseTimeout: time.Second, MaxFetchBufferSize: 1})
			defer client.Close()
			cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{UID: true, BinarySection: []*imap.FetchItemBinarySection{{}}})
			if cancel {
				select {
				case <-offered:
				case <-time.After(time.Second):
					t.Fatal("server did not offer the partial literal")
				}
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
			}
			msgs, err := cmd.Collect()
			if len(msgs) != 0 || (cancel && !errors.Is(err, net.ErrClosed)) || (!cancel && err != nil) {
				t.Errorf("ignored UID FETCH returned %d messages, %v", len(msgs), err)
			}
			client.fetchBufMutex.Lock()
			inUse := client.fetchBufInUse
			client.fetchBufMutex.Unlock()
			if inUse != 0 {
				t.Errorf("ignored response reserved %d bytes", inUse)
			}
			if !cancel {
				if err := client.Noop().Wait(); err != nil {
					t.Errorf("NOOP after ignored literal: %v", err)
				}
			}
		})
	}
}
