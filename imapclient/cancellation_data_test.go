package imapclient

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

func TestConcurrentTypedWaitDuringClientClose(t *testing.T) {
	offered := make(chan struct{})
	client := newSelectedCancellationClient(t, nil, func(conn net.Conn, br *bufio.Reader) {
		if _, _, err := readCancellationCommand(br); err != nil {
			return
		}
		const response = "* STATUS INBOX (MESSAGES 7 UIDNEXT 42)\r\n"
		if _, err := io.WriteString(conn, response); err != nil {
			return
		}
		close(offered)
		// Keep publishing untagged data until cancellation. The typed Wait
		// result must remain private until the decoder has stopped writing it.
		io.WriteString(conn, strings.Repeat(response, 10000))
		io.Copy(io.Discard, br)
	})
	cmd := client.Status("INBOX", &imap.StatusOptions{NumMessages: true, UIDNext: true})
	select {
	case <-offered:
	case <-time.After(cancellationTestTimeout):
		t.Fatal("server did not send STATUS data")
	}

	const callers = 16
	start := make(chan struct{})
	done := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			data, err := cmd.Wait()
			if data != nil {
				// Exercise data publication as well as the shared error value.
				if data.Mailbox != "" && data.Mailbox != "INBOX" {
					t.Errorf("Wait mailbox = %q", data.Mailbox)
				}
				if data.UIDNext != 0 && data.UIDNext != 42 {
					t.Errorf("Wait UIDNEXT = %v", data.UIDNext)
				}
				if data.NumMessages != nil && *data.NumMessages != 7 {
					t.Errorf("Wait MESSAGES = %v", *data.NumMessages)
				}
			}
			done <- err
		}()
	}
	close(start)
	if err := client.Close(); err != nil {
		t.Fatalf("Client.Close() = %v", err)
	}
	for i := 0; i < callers; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("Wait() = %v, want cancellation", err)
			}
		case <-time.After(cancellationTestTimeout):
			t.Fatal("typed Wait did not finish after cancellation")
		}
	}
}

func TestClientCloseDuringFetchLiteral(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		name := "streamed"
		if buffered {
			name = "buffered before UID"
		}
		t.Run(name, func(t *testing.T) {
			offered := make(chan struct{})
			client := newSelectedCancellationClient(t, &Options{MaxFetchBufferSize: 16}, func(conn net.Conn, br *bufio.Reader) {
				if _, _, err := readCancellationCommand(br); err != nil {
					return
				}
				prefix := "* 1 FETCH (UID 42 BODY[] {16}\r\nabc"
				if buffered {
					prefix = "* 1 FETCH (BODY[] {16}\r\nabc"
				}
				if _, err := io.WriteString(conn, prefix); err != nil {
					return
				}
				close(offered)
				io.Copy(io.Discard, br)
			})
			var set imap.NumSet = imap.SeqSetNum(1)
			if buffered {
				set = imap.UIDSetNum(42)
			}
			cmd := client.Fetch(set, &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{}}})
			collected := make(chan error, 1)
			go func() {
				_, err := cmd.Collect()
				collected <- err
			}()
			select {
			case <-offered:
			case <-time.After(cancellationTestTimeout):
				t.Fatal("server did not begin the FETCH literal")
			}
			if buffered {
				awaitCancellationCondition(t, "reserved partial literal budget", func() bool {
					client.fetchBufMutex.Lock()
					defer client.fetchBufMutex.Unlock()
					return client.fetchBufInUse == 16
				})
			}
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("Client.Close() = %v", err)
				}
			case <-time.After(cancellationTestTimeout):
				t.Fatal("Client.Close blocked on the partial literal")
			}
			select {
			case err := <-collected:
				if err == nil {
					t.Fatal("partial literal was collected without an error")
				}
			case <-time.After(cancellationTestTimeout):
				t.Fatal("FETCH Collect blocked after cancellation")
			}
			client.fetchBufMutex.Lock()
			inUse := client.fetchBufInUse
			client.fetchBufMutex.Unlock()
			if inUse != 0 {
				t.Errorf("partial literal retains %d buffered bytes", inUse)
			}
		})
	}
}

func TestClientCloseThenDiscardBufferedFetch(t *testing.T) {
	client := newSelectedCancellationClient(t, &Options{MaxFetchBufferSize: 4}, func(conn net.Conn, br *bufio.Reader) {
		if _, _, err := readCancellationCommand(br); err != nil {
			return
		}
		io.WriteString(conn, "* 1 FETCH (BODY[] {4}\r\ndata UID 42)\r\n")
		io.Copy(io.Discard, br)
	})
	cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	// The message has a consumer (the command's queue), but that consumer has
	// not read its buffered body. The command still owns the budget on Close.
	awaitCancellationCondition(t, "queued buffered FETCH message", func() bool { return len(cmd.msgs) != 0 })
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	discarded := make(chan error, 1)
	go func() { discarded <- cmd.Close() }()
	select {
	case err := <-discarded:
		if err == nil {
			t.Error("FETCH command completed successfully after cancellation")
		}
	case <-time.After(cancellationTestTimeout):
		t.Fatal("discarding a cancelled buffered message blocked")
	}
	client.fetchBufMutex.Lock()
	inUse := client.fetchBufInUse
	client.fetchBufMutex.Unlock()
	if inUse != 0 {
		t.Errorf("discarded message retains %d buffered bytes", inUse)
	}
}
