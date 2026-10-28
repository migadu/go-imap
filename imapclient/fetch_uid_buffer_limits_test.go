package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Every pre-UID body counts toward one response budget, including a quoted
// body and literal8. Exactly filling the budget must work without double-counting.
func TestUIDFetchBufferAggregateLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attrs   string
		limit   int64
		wantErr bool
	}{
		{"exact boundary", "BODY[1] {3}\r\none BINARY[2] ~{3}\r\ntwo", 6, false},
		{"multiple literals overflow", "BODY[1] {3}\r\none BINARY[2] ~{3}\r\ntwo", 5, true},
		{"quoted body overflow", "BODY[1] {3}\r\none BODY[2] \"two\"", 5, true},
		{"single literal overflow", "BODY[] {6}\r\nabcdef", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: tc.limit}, func(conn net.Conn, reader *bufio.Reader) error {
				tag, err := readUIDFetchWireCommand(reader)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, "* 1 FETCH (%s UID 42)\r\n%s OK done\r\n", tc.attrs, tag)
				if tc.wantErr {
					return nil
				} // Rejection may close the pipe during this write.
				return err
			})
			messages, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{
				BodySection:   []*imap.FetchItemBodySection{{Part: []int{1}}, {Part: []int{2}}},
				BinarySection: []*imap.FetchItemBinarySection{{Part: []int{2}}},
			}).Collect()
			if tc.wantErr {
				requireUIDBufferLimitError(t, err)
				return
			}
			if err != nil || len(messages) != 1 {
				t.Fatalf("Collect() returned %d messages, %v", len(messages), err)
			}
			if got := string(messages[0].FindBodySection(&imap.FetchItemBodySection{Part: []int{1}})); got != "one" {
				t.Errorf("first body = %q, want one", got)
			}
			if got := string(messages[0].FindBinarySection(&imap.FetchItemBinarySection{Part: []int{2}})); got != "two" {
				t.Errorf("binary body = %q, want two", got)
			}
		})
	}
}

// ENVELOPE and BODYSTRUCTURE can contain literals too. Limiting just BODY[]
// permits the server to retain arbitrarily many metadata literal payloads.
func TestUIDFetchBufferMetadataLiteralBudget(t *testing.T) {
	for _, tc := range []struct {
		name, attrs string
		limit       int64
		wantErr     bool
	}{
		{"envelope exact", "ENVELOPE ({3}\r\nday {3}\r\nsub NIL NIL NIL NIL NIL NIL NIL NIL)", 6, false},
		{"envelope overflow", "ENVELOPE ({3}\r\nday {3}\r\nsub NIL NIL NIL NIL NIL NIL NIL NIL)", 5, true},
		{"body plus envelope", "BODY[] {3}\r\nabc ENVELOPE (NIL {3}\r\nsub NIL NIL NIL NIL NIL NIL NIL NIL)", 5, true},
		{"bodystructure overflow", "BODYSTRUCTURE ({4}\r\nTEXT {5}\r\nPLAIN NIL NIL NIL \"7BIT\" 0 0)", 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: tc.limit}, func(conn net.Conn, reader *bufio.Reader) error {
				tag, err := readUIDFetchWireCommand(reader)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, "* 1 FETCH (%s UID 42)\r\n%s OK done\r\n", tc.attrs, tag)
				if tc.wantErr {
					return nil
				}
				return err
			})
			messages, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{Envelope: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true}, BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
			if tc.wantErr {
				requireUIDBufferLimitError(t, err)
				return
			}
			if err != nil || len(messages) != 1 {
				t.Fatalf("Collect() returned %d messages, %v", len(messages), err)
			}
			if messages[0].Envelope == nil || messages[0].Envelope.Subject != "sub" {
				t.Errorf("Envelope = %+v, want subject sub", messages[0].Envelope)
			}
		})
	}
}

func TestUIDFetchBufferRejectsOversizeBeforePayload(t *testing.T) {
	client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: 4}, func(conn net.Conn, reader *bufio.Reader) error {
		if _, err := readUIDFetchWireCommand(reader); err != nil {
			return err
		}
		if _, err := io.WriteString(conn, "* 1 FETCH (BODY[] {5}\r\n"); err != nil {
			return nil
		}
		// Never send the payload: validation must happen before buffering it.
		_, _ = reader.ReadByte()
		return nil
	})
	_, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	requireUIDBufferLimitError(t, err)
}

func TestUIDFetchBufferItemLimit(t *testing.T) {
	for _, tc := range []struct{ name, attr string }{
		{"empty bodies", "BODY[] {0}\r\n "},
		{"metadata", "BINARY.SIZE[1] 0 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: 1}, func(conn net.Conn, reader *bufio.Reader) error {
				tag, err := readUIDFetchWireCommand(reader)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(conn, "* 1 FETCH (%sUID 42)\r\n%s OK done\r\n", strings.Repeat(tc.attr, 4097), tag)
				return nil
			})
			_, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
			if err == nil {
				t.Fatal("staging 4097 items succeeded, want an explicit item-limit error")
			}
			if isTimeout(err) || !strings.Contains(err.Error(), "4096 items") {
				t.Fatalf("error = %v, want an explicit 4096-item rejection", err)
			}
		})
	}
}

func TestUIDFetchBufferLiteralReadErrors(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		name := "timeout"
		if truncated {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: 4, LiteralReadTimeout: 100 * time.Millisecond}, func(conn net.Conn, reader *bufio.Reader) error {
				if _, err := readUIDFetchWireCommand(reader); err != nil {
					return err
				}
				if _, err := io.WriteString(conn, "* 1 FETCH (BODY[] {4}\r\nab"); err != nil {
					return err
				}
				if !truncated {
					_, _ = reader.ReadByte()
				}
				return nil // Closing with two bytes missing must surface the truncation.
			})
			_, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
			if err == nil {
				t.Fatal("incomplete literal returned success")
			}
			if truncated {
				if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
					t.Errorf("truncated body error = %v, want EOF", err)
				}
			} else if !isTimeout(err) {
				t.Errorf("stalled body error = %v, want timeout", err)
			}
		})
	}
}

func TestUIDFetchBufferDiscardAndBudgetReset(t *testing.T) {
	client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: 4}, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, "* 1 FETCH (BODY[] {4}\r\none! UID 41)\r\n* 2 FETCH (BODY[] {4}\r\ntwo! UID 42)\r\n%s OK done\r\n", tag)
		return err
	})
	cmd := client.Fetch(imap.UIDSetNum(41, 42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	if first := cmd.Next(); first == nil {
		t.Fatalf("first Next() = nil: %v", cmd.Close())
	}
	// Next must discard the unread first body and release decoder backpressure.
	second := cmd.Next()
	if second == nil {
		t.Fatalf("second Next() = nil: %v", cmd.Close())
	}
	msg, err := second.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if msg.UID != 42 || string(msg.FindBodySection(&imap.FetchItemBodySection{})) != "two!" {
		t.Errorf("second message UID=%d body=%q", msg.UID, msg.FindBodySection(&imap.FetchItemBodySection{}))
	}
	if err := cmd.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

func TestUIDFetchBufferCloseUnread(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		name := "command"
		if closeClient {
			name = "connection"
		}
		t.Run(name, func(t *testing.T) {
			client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: 4}, func(conn net.Conn, reader *bufio.Reader) error {
				tag, err := readUIDFetchWireCommand(reader)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, "* 1 FETCH (BODY[] {4}\r\ndata UID 42)\r\n%s OK done\r\n", tag)
				if closeClient {
					return nil
				}
				return err
			})
			cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
			if msg := cmd.Next(); msg == nil {
				t.Fatalf("Next() = nil: %v", cmd.Close())
			}
			done := make(chan error, 1)
			go func() {
				if closeClient {
					done <- client.Close()
				} else {
					done <- cmd.Close()
				}
			}()
			select {
			case err := <-done:
				if !closeClient && err != nil {
					t.Errorf("Close() = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Close blocked on an unread buffered body")
			}
		})
	}
}

// Before the caller consumes/discards the first replayed body, the decoder must
// not buffer subsequent responses into the command's 128-message queue.
func TestUIDFetchBufferReplayBackpressure(t *testing.T) {
	secondStarted := make(chan struct{})
	secondWritten := make(chan struct{})
	payload := strings.Repeat("x", 32*1024) // Larger than the decoder's read-ahead buffer.
	client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: int64(len(payload))}, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		if _, err = io.WriteString(conn, "* 1 FETCH (BODY[] {4}\r\none! UID 41)\r\n"); err != nil {
			return err
		}
		close(secondStarted)
		if _, err = fmt.Fprintf(conn, "* 2 FETCH (BODY[] {%d}\r\n%s UID 42)\r\n", len(payload), payload); err != nil {
			return err
		}
		close(secondWritten)
		_, err = fmt.Fprintf(conn, "%s OK done\r\n", tag)
		return err
	})
	cmd := client.Fetch(imap.UIDSetNum(41, 42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	if first := cmd.Next(); first == nil {
		t.Fatalf("Next() = nil: %v", cmd.Close())
	}
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not begin second response")
	}
	select {
	case <-secondWritten:
		t.Error("server delivered a second buffered response while the first body was unread")
	case <-time.After(100 * time.Millisecond):
	}
	second := cmd.Next() // Discards first; the server can now deliver the second.
	if second == nil {
		t.Fatalf("second Next() = nil: %v", cmd.Close())
	}
	msg, err := second.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if msg.UID != 42 || string(msg.FindBodySection(&imap.FetchItemBodySection{})) != payload {
		t.Error("second message did not survive backpressure")
	}
	if err := cmd.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

func requireUIDBufferLimitError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("exceeded MaxFetchBufferSize without an error")
	}
	if isTimeout(err) || !strings.Contains(err.Error(), "MaxFetchBufferSize") {
		t.Fatalf("error = %v, want MaxFetchBufferSize rejection", err)
	}
}

func isTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

func newUIDBufferLimitClient(t *testing.T, options *imapclient.Options, script func(net.Conn, *bufio.Reader) error) *imapclient.Client {
	t.Helper()
	cc, sc := net.Pipe()
	if err := sc.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		defer sc.Close()
		if _, err := io.WriteString(sc, "* OK [CAPABILITY IMAP4rev1 BINARY] ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		serverDone <- script(sc, bufio.NewReader(sc))
	}()
	var opts imapclient.Options
	if options != nil {
		opts = *options
	}
	if opts.ResponseTimeout <= 0 {
		opts.ResponseTimeout = 2 * time.Second
	}
	if opts.LiteralReadTimeout <= 0 {
		opts.LiteralReadTimeout = 2 * time.Second
	}
	client := imapclient.New(cc, &opts)
	t.Cleanup(func() {
		// Closing the socket alone cannot interrupt local consumption waits.
		go client.Close()
		cc.Close()
		sc.Close()
		select {
		case err := <-serverDone:
			if err != nil && !t.Failed() {
				t.Errorf("wire server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("wire server did not stop")
		}
		select {
		case <-client.Closed():
		case <-time.After(2 * time.Second):
			t.Error("client response reader did not stop")
		}
	})
	return client
}

// Metadata literals bypass the BODY reader, so they need message-consumption
// backpressure as well; otherwise up to 128 full response budgets can queue.
func TestUIDFetchBufferMetadataBackpressure(t *testing.T) {
	secondStarted := make(chan struct{})
	secondWritten := make(chan struct{})
	subject := strings.Repeat("s", 32*1024)
	client := newUIDBufferLimitClient(t, &imapclient.Options{MaxFetchBufferSize: int64(len(subject))}, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		if _, err = io.WriteString(conn, "* 1 FETCH (ENVELOPE (NIL {4}\r\none! NIL NIL NIL NIL NIL NIL NIL NIL) UID 41)\r\n"); err != nil {
			return err
		}
		close(secondStarted)
		if _, err = fmt.Fprintf(conn, "* 2 FETCH (ENVELOPE (NIL {%d}\r\n%s NIL NIL NIL NIL NIL NIL NIL NIL) UID 42)\r\n", len(subject), subject); err != nil {
			return err
		}
		close(secondWritten)
		_, err = fmt.Fprintf(conn, "%s OK done\r\n", tag)
		return err
	})
	cmd := client.Fetch(imap.UIDSetNum(41, 42), &imap.FetchOptions{Envelope: true})
	if first := cmd.Next(); first == nil {
		t.Fatalf("Next() = nil: %v", cmd.Close())
	}
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not begin second response")
	}
	select {
	case <-secondWritten:
		t.Error("second metadata response was buffered while first was unconsumed")
	case <-time.After(100 * time.Millisecond):
	}
	second := cmd.Next()
	if second == nil {
		t.Fatalf("second Next() = nil: %v", cmd.Close())
	}
	msg, err := second.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if msg.UID != 42 || msg.Envelope == nil || msg.Envelope.Subject != subject {
		t.Error("second envelope did not survive backpressure")
	}
	if err := cmd.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

// Metadata without literal payload has always been queueable. Staging FLAGS
// until UID must not impose literal-consumption barriers on ordinary pipelining.
func TestUIDFetchBufferFlagsOnlyRemainPipelineable(t *testing.T) {
	client := newUIDBufferLimitClient(t, nil, func(conn net.Conn, reader *bufio.Reader) error {
		first, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		second, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, "* 1 FETCH (FLAGS (\\Seen) UID 41)\r\n%s OK first\r\n* 2 FETCH (FLAGS (\\Flagged) UID 42)\r\n%s OK second\r\n", first, second)
		return err
	})
	first := client.Fetch(imap.UIDSetNum(41), &imap.FetchOptions{Flags: true})
	second := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{Flags: true})
	type result struct {
		messages []*imapclient.FetchMessageBuffer
		err      error
	}
	resultCh := make(chan result, 1)
	go func() { messages, err := second.Collect(); resultCh <- result{messages, err} }()
	select {
	case got := <-resultCh:
		if got.err != nil || len(got.messages) != 1 || got.messages[0].UID != 42 {
			t.Fatalf("second command = %v, %v", got.messages, got.err)
		}
	case <-time.After(2 * time.Second):
		go client.Close()
		t.Fatal("flags-only first response blocks consumption of second command")
	}
	messages, err := first.Collect()
	if err != nil || len(messages) != 1 || messages[0].UID != 41 {
		t.Fatalf("first command = %v, %v", messages, err)
	}
}

// Time spent with buffered metadata in application code is not a stalled
// server response. The wire deadline must restart when consumption resumes.
func TestUIDFetchBufferMetadataConsumptionRestoresDeadline(t *testing.T) {
	client := newUIDBufferLimitClient(t, &imapclient.Options{ResponseTimeout: 100 * time.Millisecond, LiteralReadTimeout: 2 * time.Second}, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		if _, err = io.WriteString(conn, "* 1 FETCH (ENVELOPE (NIL {4}\r\nslow NIL NIL NIL NIL NIL NIL NIL NIL) UID 42)"); err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, "\r\n%s OK done\r\n", tag)
		return err
	})
	cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{Envelope: true})
	msg := cmd.Next()
	if msg == nil {
		t.Fatalf("Next() = nil: %v", cmd.Close())
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := msg.Collect(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("slow local consumption caused a wire error: %v", err)
	}
}
