package imapclient

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

// The response bytes have arrived, but the decoder has not yet parsed their
// tagged success. Cancellation must win state transitions from that response.
func TestClientCloseBeforeTaggedSuccessKeepsLogout(t *testing.T) {
	for _, name := range []string{"LOGIN", "SELECT"} {
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			gate := &completionResponseGate{entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(gate.release) }); clientConn.Close(); serverConn.Close() })
			go func() {
				defer serverConn.Close()
				greeting := "OK"
				if name == "SELECT" {
					greeting = "PREAUTH"
				}
				io.WriteString(serverConn, "* "+greeting+" [CAPABILITY IMAP4rev1] ready\r\n")
				br := bufio.NewReader(serverConn)
				tag, got, err := readCancellationCommand(br)
				if err != nil {
					return
				}
				if got != name {
					t.Errorf("command = %s, want %s", got, name)
					return
				}
				prefix := ""
				if name == "SELECT" {
					prefix = "* FLAGS ()\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n"
				}
				io.WriteString(serverConn, prefix+tag+" OK [CAPABILITY IMAP4rev1] completion-gate\r\n")
				io.Copy(io.Discard, br)
			}()
			client := New(clientConn, &Options{DebugWriter: gate})
			var wait func() error
			if name == "LOGIN" {
				cmd := client.Login("user", "pass")
				wait = cmd.Wait
			} else {
				cmd := client.Select("INBOX", nil)
				wait = func() error { _, err := cmd.Wait(); return err }
			}
			select {
			case <-gate.entered:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("tagged success did not reach the decoder gate")
			}
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case <-client.closeCh:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("Close did not signal cancellation")
			}
			if state := client.State(); state != imap.ConnStateLogout {
				t.Fatalf("state after cancellation = %v, want logout", state)
			}
			release.Do(func() { close(gate.release) })
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Close: %v", err)
				}
			case <-time.After(cancellationTestTimeout):
				t.Fatal("Close did not finish after releasing tagged success")
			}
			if err := wait(); !errors.Is(err, net.ErrClosed) {
				t.Errorf("command completed with %v after cancellation, want net.ErrClosed", err)
			}
			if state := client.State(); state != imap.ConnStateLogout {
				t.Errorf("late tagged success restored state %v", state)
			}
			if mailbox := client.Mailbox(); mailbox != nil {
				t.Errorf("late tagged success restored mailbox %+v", mailbox)
			}
		})
	}
}

type completionResponseGate struct {
	entered, release chan struct{}
	once             sync.Once
}

func (g *completionResponseGate) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "completion-gate") {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return len(p), nil
}

// A command's outcome is published once and broadcast to every waiter. A
// tagged NO or cancellation must not be consumed by just one goroutine.
func TestCommandConcurrentWaitPublishesSameResult(t *testing.T) {
	for _, outcome := range []string{"OK", "NO", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			received := make(chan struct{})
			response := make(chan string, 1)
			client := newSelectedCancellationClient(t, nil, func(conn net.Conn, br *bufio.Reader) {
				tag, name, err := readCancellationCommand(br)
				if err != nil {
					return
				}
				if name != "NOOP" {
					t.Errorf("command = %s, want NOOP", name)
					return
				}
				close(received)
				if status := <-response; status != "" {
					io.WriteString(conn, tag+" "+status+"\r\n")
				}
				io.Copy(io.Discard, br)
			})
			var respond sync.Once
			send := func(status string) { respond.Do(func() { response <- status }) }
			t.Cleanup(func() { send(""); client.Close() })
			cmd := client.Noop()
			select {
			case <-received:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("server did not receive NOOP")
			}
			const waiters = 32
			start := make(chan struct{})
			result := make(chan error, waiters)
			var ready sync.WaitGroup
			ready.Add(waiters)
			for i := 0; i < waiters; i++ {
				go func() { ready.Done(); <-start; result <- cmd.Wait() }()
			}
			ready.Wait()
			close(start)
			switch outcome {
			case "OK":
				send("OK completed")
			case "NO":
				send("NO [UNAVAILABLE] waiter-sentinel")
			case "cancel":
				if err := client.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			for i := 0; i < waiters; i++ {
				select {
				case err := <-result:
					switch outcome {
					case "OK":
						if err != nil {
							t.Errorf("waiter %d: %v", i, err)
						}
					case "NO":
						var imapErr *imap.Error
						if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo || imapErr.Text != "waiter-sentinel" {
							t.Errorf("waiter %d: %v, want same tagged NO", i, err)
						}
					case "cancel":
						if !errors.Is(err, net.ErrClosed) {
							t.Errorf("waiter %d: %v, want net.ErrClosed", i, err)
						}
					}
				case <-time.After(cancellationTestTimeout):
					t.Fatal("concurrent command waiter did not finish")
				}
			}
		})
	}
}
