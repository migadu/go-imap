package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// awaitDeadlineWithin waits until the client has armed a read deadline no more
// than max away, and fails otherwise. It reports the deadline it settled on.
func awaitDeadlineWithin(t *testing.T, rec *deadlineConn, max time.Duration) time.Duration {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last time.Duration
	for time.Now().Before(deadline) {
		if got, ok := rec.lastDeadline(); ok {
			last = time.Until(got)
			if last <= max {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("read deadline is %v away, want at most %v", last.Round(time.Millisecond), max)
	return 0
}

// awaitDeadlineAtLeast waits until the client has armed a read deadline at
// least min away. The literal deadline is armed only just before the literal is
// handed to the consumer, so a single sample can still see the response
// deadline from earlier in the same response.
func awaitDeadlineAtLeast(t *testing.T, rec *deadlineConn, min time.Duration) time.Duration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last time.Duration
	for time.Now().Before(deadline) {
		if got, ok := rec.lastDeadline(); ok {
			last = time.Until(got)
			if last >= min {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("read deadline is %v away, want at least %v", last.Round(time.Millisecond), min)
	return 0
}

// TestOptionsResponseTimeoutApplied checks that Options.ResponseTimeout reaches
// the socket: the deadline armed while a command is pending must be the
// configured one, not the 30s default.
func TestOptionsResponseTimeoutApplied(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	cmdCh := make(chan struct{}, 1)
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")
		for {
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
			select {
			case cmdCh <- struct{}{}:
			default:
			}
		}
	}()

	recorder := &deadlineConn{Conn: clientConn}
	client := imapclient.New(recorder, &imapclient.Options{ResponseTimeout: 2 * time.Second})
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	client.Noop()
	<-cmdCh

	// Well under the 30s default, so this can only be the configured value.
	got := awaitDeadlineWithin(t, recorder, 5*time.Second)
	if got <= 0 {
		t.Fatalf("read deadline already expired (%v)", got)
	}
}

// TestOptionsResponseTimeoutDefaultUnchanged is the compatibility guard: an
// Options value that does not mention the new fields -- which is every caller
// written before them -- must keep the 30s default rather than losing its
// deadline.
func TestOptionsResponseTimeoutDefaultUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options *imapclient.Options
	}{
		{name: "nil options", options: nil},
		{name: "empty options", options: &imapclient.Options{}},
		{name: "unrelated field set", options: &imapclient.Options{MaxLiteralSize: 1 << 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()

			cmdCh := make(chan struct{}, 1)
			go func() {
				defer serverConn.Close()
				br := bufio.NewReader(serverConn)
				io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")
				for {
					if _, err := br.ReadString('\n'); err != nil {
						return
					}
					select {
					case cmdCh <- struct{}{}:
					default:
					}
				}
			}()

			recorder := &deadlineConn{Conn: clientConn}
			client := imapclient.New(recorder, tc.options)
			defer client.Close()

			if err := client.WaitGreeting(); err != nil {
				t.Fatalf("WaitGreeting() = %v", err)
			}
			client.Noop()
			<-cmdCh

			// The default is 30s. Crucially it must also not be unbounded:
			// a cleared deadline shows up as the zero time, i.e. far in the past.
			got := awaitDeadlineWithin(t, recorder, 40*time.Second)
			if got < 20*time.Second {
				t.Errorf("read deadline is %v away, want ~30s: the default was lost", got.Round(time.Second))
			}
		})
	}
}

// TestOptionsLiteralReadTimeoutApplied checks the second field: while a literal
// is open the armed deadline must be the configured literal timeout, which is
// distinguishable from both the response timeout and the 5 minute default.
func TestOptionsLiteralReadTimeoutApplied(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	const literal = "0123456789"
	literalOpen := make(chan struct{})
	finishLiteral := make(chan struct{})

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if strings.EqualFold(fields[1], "FETCH") {
				io.WriteString(serverConn, "* 1 FETCH (BODY[] {"+strconv.Itoa(len(literal))+"}\r\n")
				close(literalOpen)
				<-finishLiteral
				io.WriteString(serverConn, literal+")\r\n")
				io.WriteString(serverConn, fields[0]+" OK FETCH completed\r\n")
				continue
			}
			io.WriteString(serverConn, fields[0]+" OK completed\r\n")
		}
	}()

	recorder := &deadlineConn{Conn: clientConn}
	client := imapclient.New(recorder, &imapclient.Options{
		ResponseTimeout:    2 * time.Second,
		LiteralReadTimeout: 45 * time.Second,
	})
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	})
	resCh := make(chan error, 1)
	go func() {
		_, err := fetchCmd.Collect()
		resCh <- err
	}()

	<-literalOpen
	// 45s is chosen to sit clear of every value it could be confused with: the
	// 2s configured response timeout, the 30s response default, and the 5
	// minute literal default. Only the configured literal timeout lands in
	// (35s, 90s), so this cannot pass by observing one of the others.
	got := awaitDeadlineAtLeast(t, recorder, 35*time.Second)
	if got > 90*time.Second {
		t.Errorf("read deadline is %v away while a literal is open, want ~45s: the literal default was used instead of the configured value", got.Round(time.Second))
	}

	close(finishLiteral)
	if err := <-resCh; err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	}
}
