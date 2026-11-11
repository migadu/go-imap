package imapclient

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

const cancellationTestTimeout = 3 * time.Second

// The scripts below establish a real selected session before exercising
// cancellation. In particular, the EXPUNGE and EXISTS responses are valid
// mailbox data rather than responses the client should discard for its state.
func newSelectedCancellationClient(t *testing.T, options *Options, script func(net.Conn, *bufio.Reader)) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		if _, err := io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 LIST-EXTENDED LIST-STATUS] ready\r\n"); err != nil {
			return
		}
		tag, name, err := readCancellationCommand(br)
		if err != nil {
			return
		}
		if name != "SELECT" {
			t.Errorf("first command = %s, want SELECT", name)
			return
		}
		if _, err := io.WriteString(serverConn, "* 256 EXISTS\r\n"+tag+" OK selected\r\n"); err != nil {
			return
		}
		script(serverConn, br)
	}()
	client := New(clientConn, options)
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}
	return client
}

func readCancellationCommand(br *bufio.Reader) (tag, name string, err error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("invalid command %q", line)
	}
	return fields[0], fields[1], nil
}

type cancellationStreamCase struct {
	name          string
	status, final bool
}

var cancellationStreamCases = []cancellationStreamCase{
	{name: "EXPUNGE"},
	{name: "LIST"},
	{name: "LSUB"},
	{name: "LIST-STATUS", status: true},
	{name: "LIST-final", status: true, final: true},
}

type cancellationStreamProbe struct {
	command  command
	capacity int
	full     func() bool
	drain    func() error
}

func (tc cancellationStreamCase) start(client *Client) cancellationStreamProbe {
	if tc.name == "EXPUNGE" {
		cmd := client.Expunge()
		return cancellationStreamProbe{cmd, cap(cmd.seqNums), func() bool { return len(cmd.seqNums) == cap(cmd.seqNums) }, cmd.Close}
	}
	var cmd *ListCommand
	if tc.name == "LSUB" {
		cmd = client.Lsub("", "*")
	} else {
		var options *imap.ListOptions
		if tc.status {
			options = &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true}}
		}
		cmd = client.List("", "*", options)
	}
	return cancellationStreamProbe{cmd, cap(cmd.mailboxes), func() bool { return len(cmd.mailboxes) == cap(cmd.mailboxes) }, cmd.Close}
}
func (tc cancellationStreamCase) server(t *testing.T, count <-chan int, offered chan<- struct{}) func(net.Conn, *bufio.Reader) {
	return func(conn net.Conn, br *bufio.Reader) {
		tag, name, err := readCancellationCommand(br)
		if err != nil {
			return
		}
		want := tc.name
		if tc.status {
			want = "LIST"
		}
		if name != want {
			t.Errorf("command = %s, want %s", name, want)
			return
		}
		capacity := <-count
		for i := 0; i <= capacity; i++ {
			var response string
			switch {
			case tc.name == "EXPUNGE":
				response = "* 1 EXPUNGE\r\n"
			case tc.name == "LSUB":
				response = fmt.Sprintf("* LSUB () \"/\" \"m%d\"\r\n", i)
			case tc.final && i == capacity:
				// LIST-STATUS need not return STATUS for a non-selectable mailbox.
				// This last mailbox is flushed by completeCommand after tagged OK.
				response = fmt.Sprintf("* LIST (\\Noselect) \"/\" \"m%d\"\r\n", i)
			default:
				response = fmt.Sprintf("* LIST () \"/\" \"m%d\"\r\n", i)
				if tc.status {
					response += fmt.Sprintf("* STATUS \"m%d\" (MESSAGES 0)\r\n", i)
				}
			}
			if _, err := io.WriteString(conn, response); err != nil {
				return
			}
		}
		if tc.final {
			if _, err := io.WriteString(conn, tag+" OK list complete\r\n"); err != nil {
				return
			}
		}
		close(offered)
		io.Copy(io.Discard, br)
	}
}

func awaitCancellationCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(cancellationTestTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
func assertNoReaderPanic(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "panic reading response") {
		t.Fatalf("cancellation caused a reader panic: %v", err)
	}
}

// Closing the connection must release a reader blocked publishing a stream,
// including the final LIST mailbox after its command left the pending queue.
func TestClientCloseWithUndrainedStreams(t *testing.T) {
	for _, tc := range cancellationStreamCases {
		t.Run(tc.name, func(t *testing.T) {
			count := make(chan int, 1)
			offered := make(chan struct{})
			client := newSelectedCancellationClient(t, nil, tc.server(t, count, offered))
			stream := tc.start(client)
			count <- stream.capacity
			select {
			case <-offered:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("server did not offer enough results")
			}
			awaitCancellationCondition(t, "full result buffer", stream.full)
			if tc.final {
				awaitCancellationCondition(t, "LIST tagged completion", func() bool {
					return client.findPendingCmdFunc(func(cmd command) bool { return cmd == stream.command }) == nil
				})
			}
			drained := make(chan error, 1)
			var drainOnce sync.Once
			drain := func() { drainOnce.Do(func() { go func() { drained <- stream.drain() }() }) }
			defer drain()
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case err := <-closed:
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				// Release the old implementation as well, so a failing regression does
				// not leak a goroutine or leave a test waiting for its binary timeout.
				drain()
				select {
				case <-closed:
				case <-time.After(cancellationTestTimeout):
				}
				t.Fatal("Client.Close blocked until the stream consumer resumed")
			}
			drain()
			select {
			case err := <-drained:
				if err == nil {
					t.Fatal("aborted stream completed successfully")
				}
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				t.Fatal("stream did not finish after Client.Close")
			}
		})
	}
}

// An encoder error occurs outside the read goroutine. It must cancel blocked
// delivery without closing a result channel while the reader is sending to it.
func TestEncoderFailureWithUndrainedStreams(t *testing.T) {
	for _, tc := range cancellationStreamCases {
		t.Run(tc.name, func(t *testing.T) {
			count := make(chan int, 1)
			offered := make(chan struct{})
			client := newSelectedCancellationClient(t, nil, tc.server(t, count, offered))
			stream := tc.start(client)
			count <- stream.capacity
			select {
			case <-offered:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("server did not offer enough results")
			}
			awaitCancellationCondition(t, "full result buffer", stream.full)
			if tc.final {
				awaitCancellationCondition(t, "LIST tagged completion", func() bool {
					return client.findPendingCmdFunc(func(cmd command) bool { return cmd == stream.command }) == nil
				})
			}
			failed := make(chan error, 1)
			go func() {
				// Whitespace cannot occur in a flag atom. Encoder.Flag fails locally,
				// making commandEncoder.flush signal a fatal error from this goroutine.
				cmd := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Flags: []imap.Flag{"invalid flag"}}, nil)
				failed <- cmd.Close()
			}()
			drained := make(chan error, 1)
			var drainOnce sync.Once
			drain := func() { drainOnce.Do(func() { go func() { drained <- stream.drain() }() }) }
			defer drain()
			select {
			case err := <-failed:
				if err == nil {
					t.Error("invalid STORE flag completed successfully")
				}
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				drain()
				select {
				case <-failed:
				case <-time.After(cancellationTestTimeout):
				}
				t.Fatal("encoder failure blocked on an unrelated stream consumer")
			}
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case err := <-closed:
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				drain()
				select {
				case <-closed:
				case <-time.After(cancellationTestTimeout):
				}
				t.Fatal("reader did not terminate after encoder failure")
			}
			drain()
			select {
			case err := <-drained:
				if err == nil {
					t.Error("stream interrupted by encoder failure completed successfully")
				}
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				t.Fatal("stream did not finish after encoder failure")
			}
		})
	}
}

// Handlers run on the reader goroutine. Waiting in one is a caller error, but
// closing the client must still end pending command waits and stream readers.
func TestClientCloseReleasesHandlerCommandWait(t *testing.T) {
	for _, kind := range []string{"NOOP", "FETCH", "LIST", "EXPUNGE"} {
		t.Run(kind, func(t *testing.T) {
			var client *Client
			ready := make(chan struct{})
			issued := make(chan struct{})
			handlerDone := make(chan error, 1)
			options := &Options{UnilateralDataHandler: &UnilateralDataHandler{Mailbox: func(*UnilateralDataMailbox) {
				<-ready
				var wait func() error
				switch kind {
				case "NOOP":
					cmd := client.Noop()
					wait = cmd.Wait
				case "FETCH":
					cmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{UID: true})
					wait = func() error { _, err := cmd.Collect(); return err }
				case "LIST":
					cmd := client.List("", "*", nil)
					wait = func() error { _, err := cmd.Collect(); return err }
				case "EXPUNGE":
					cmd := client.Expunge()
					wait = func() error { _, err := cmd.Collect(); return err }
				}
				close(issued)
				handlerDone <- wait()
			}}}
			client = newSelectedCancellationClient(t, options, func(conn net.Conn, br *bufio.Reader) {
				io.WriteString(conn, "* 257 EXISTS\r\n")
				// Consume the command without completing it. The callback is now waiting
				// for a reply which only this same reader goroutine could dispatch.
				readCancellationCommand(br)
				io.Copy(io.Discard, br)
			})
			close(ready)
			select {
			case <-issued:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("handler did not issue its command")
			}
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case err := <-closed:
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				// The reader is stopped in the callback, so completing pending commands
				// here is safe as failure cleanup for the old implementation.
				go client.closeWithError(net.ErrClosed)
				select {
				case <-closed:
				case <-time.After(cancellationTestTimeout):
				}
				t.Fatal("Client.Close waited for the handler's pending command")
			}
			select {
			case err := <-handlerDone:
				if err == nil {
					t.Fatal("handler's command succeeded after client cancellation")
				}
				assertNoReaderPanic(t, err)
			case <-time.After(cancellationTestTimeout):
				t.Fatal("handler wait did not observe client cancellation")
			}
		})
	}
}

func TestConcurrentClientCloseCompletesPendingCommandOnce(t *testing.T) {
	offered := make(chan struct{})
	client := newSelectedCancellationClient(t, nil, func(conn net.Conn, br *bufio.Reader) {
		readCancellationCommand(br)
		close(offered)
		io.Copy(io.Discard, br)
	})
	cmd := client.Noop()
	<-offered
	const callers = 16
	start := make(chan struct{})
	closed := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { <-start; closed <- client.Close() }()
	}
	close(start)
	successes := 0
	for i := 0; i < callers; i++ {
		select {
		case err := <-closed:
			assertNoReaderPanic(t, err)
			if err == nil {
				successes++
			} else if !errors.Is(err, net.ErrClosed) {
				t.Errorf("concurrent Close: %v", err)
			}
		case <-time.After(cancellationTestTimeout):
			t.Fatal("concurrent Client.Close did not return")
		}
	}
	if successes != 1 {
		t.Errorf("%d Close calls succeeded, want one", successes)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("pending NOOP succeeded after client cancellation")
	}
}
