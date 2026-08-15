package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// deadlineConn records every read deadline set on it, so a test can assert how
// long the client is prepared to wait rather than actually waiting that long.
type deadlineConn struct {
	net.Conn

	mutex     sync.Mutex
	deadlines []time.Time
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.mutex.Lock()
	c.deadlines = append(c.deadlines, t)
	c.mutex.Unlock()
	return c.Conn.SetReadDeadline(t)
}

// lastDeadline returns the most recently set read deadline.
func (c *deadlineConn) lastDeadline() (time.Time, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if len(c.deadlines) == 0 {
		return time.Time{}, false
	}
	return c.deadlines[len(c.deadlines)-1], true
}

// TestClientResponseTimeoutWhileCommandPending is a regression test for the
// client waiting for the first byte of a response under the idle deadline.
//
// The response deadline was applied inside readResponse, which only runs once a
// byte has arrived. Between responses -- which is exactly where a client sits
// after sending a command -- the deadline in force was the one readResponse
// restored on its way out: idleReadTimeout, 30 minutes.
//
// So a server that accepts a command and then goes silent is not detected for
// half an hour. That defeats the common pattern of sending a periodic NOOP
// precisely to detect a dead peer: Noop().Wait() simply does not return. The
// reporter hit this with a blackholed connection where TCP retransmission ran
// for ~18 minutes.
//
// This asserts the deadline the client arms rather than waiting 30s for it to
// expire, so it stays fast and deterministic.
//
// Upstream report: emersion/go-imap#762 by WhyNotHugo.
func TestClientResponseTimeoutWhileCommandPending(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	// A server that greets and then never answers anything.
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
	client := imapclient.New(recorder, nil)
	defer client.Close()

	// Let the client settle after the greeting, so the deadline we inspect
	// below is the one it chose for waiting on our command's response.
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	cmd := client.Noop()
	<-cmdCh // the server has the NOOP; the client is now waiting for a reply

	// Give the client a moment to arm its deadline for the wait.
	deadline := time.Now().Add(2 * time.Second)
	var (
		got time.Time
		ok  bool
	)
	for time.Now().Before(deadline) {
		got, ok = recorder.lastDeadline()
		if ok && time.Until(got) <= 2*time.Minute {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("client never set a read deadline")
	}

	// respReadTimeout is 30s. Anything on the order of the 30-minute idle
	// timeout means a pending command is being waited on with no useful bound.
	if remaining := time.Until(got); remaining > 2*time.Minute {
		t.Errorf("read deadline is %v away while a command is pending, want the response timeout (~30s): a silent server goes undetected for that long", remaining.Round(time.Second))
	}

	_ = cmd
}

// TestClientIdleTimeoutWhenNothingPending is the guard: with no command in
// flight the client must keep the long deadline, or it would tear down a
// perfectly healthy connection that simply has no traffic.
func TestClientIdleTimeoutWhenNothingPending(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

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
			if len(fields) > 0 {
				io.WriteString(serverConn, fields[0]+" OK completed\r\n")
			}
		}
	}()

	recorder := &deadlineConn{Conn: clientConn}
	client := imapclient.New(recorder, nil)
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	// Run a command to completion, so the client goes back to having nothing
	// pending.
	if err := client.Noop().Wait(); err != nil {
		t.Fatalf("Noop().Wait() = %v", err)
	}

	// Once settled, the deadline must be the long one again.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := recorder.lastDeadline(); ok && time.Until(got) > 2*time.Minute {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := recorder.lastDeadline()
	t.Errorf("read deadline is %v away with nothing pending, want the idle timeout (~30min)", time.Until(got).Round(time.Second))
}

// TestClientIdleKeepsLongTimeout guards IDLE. An IDLE command is pending for
// its whole duration, but silence is exactly what IDLE is for, so it must keep
// the long deadline rather than being torn down after the response timeout.
func TestClientIdleKeepsLongTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	idlingCh := make(chan struct{}, 1)
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2 IDLE] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch {
			case len(fields) > 1 && strings.EqualFold(fields[1], "IDLE"):
				io.WriteString(serverConn, "+ idling\r\n")
				select {
				case idlingCh <- struct{}{}:
				default:
				}
			case strings.TrimSpace(line) == "DONE":
				io.WriteString(serverConn, "T1 OK IDLE completed\r\n")
			default:
				io.WriteString(serverConn, fields[0]+" OK completed\r\n")
			}
		}
	}()

	recorder := &deadlineConn{Conn: clientConn}
	client := imapclient.New(recorder, nil)
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	idleCmd, err := client.Idle()
	if err != nil {
		t.Fatalf("Idle() = %v", err)
	}
	defer idleCmd.Close()
	<-idlingCh

	// While idling, the deadline must stay long.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := recorder.lastDeadline(); ok && time.Until(got) > 2*time.Minute {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := recorder.lastDeadline()
	t.Errorf("read deadline is %v away during IDLE, want the idle timeout (~30min): IDLE would be torn down while working correctly", time.Until(got).Round(time.Second))
}

// countDeadlines returns how many deadlines were recorded, so a test can look
// at only the ones set after a given point.
func (c *deadlineConn) countDeadlines() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return len(c.deadlines)
}

// deadlinesSince returns the deadlines recorded after index i.
func (c *deadlineConn) deadlinesSince(i int) []time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return append([]time.Time(nil), c.deadlines[i:]...)
}

// TestClientLiteralNotCutShortByConcurrentCommand guards the invariant that
// makes retuning the deadline from beginCommand safe.
//
// A command may be sent from another goroutine while the read goroutine is
// midway through a response. Literals get a much longer deadline than a
// response does, so a beginCommand that shortened the deadline at that moment
// would truncate a large FETCH body. The deadline may only be retuned while the
// reader is parked between responses, which is what awaitingResp tracks.
func TestClientLiteralNotCutShortByConcurrentCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	const literal = "0123456789ABCDEF"

	literalOpen := make(chan struct{})
	finishLiteral := make(chan struct{})

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2] ready\r\n")

		var fetchTag string
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
				fetchTag = fields[0]
				// Open a literal and stop, leaving the client blocked inside
				// the response with the literal deadline armed.
				io.WriteString(serverConn, "* 1 FETCH (BODY[] {"+strconv.Itoa(len(literal))+"}\r\n")
				close(literalOpen)
				<-finishLiteral
				io.WriteString(serverConn, literal+")\r\n")
				io.WriteString(serverConn, fetchTag+" OK FETCH completed\r\n")
				continue
			}
			io.WriteString(serverConn, fields[0]+" OK completed\r\n")
		}
	}()

	recorder := &deadlineConn{Conn: clientConn}
	client := imapclient.New(recorder, nil)
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	fetchCmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	})

	// Collect concurrently: the read goroutine hands the literal to the
	// consumer, so without one the connection stalls instead of reaching the
	// state under test.
	type fetchResult struct {
		msgs []*imapclient.FetchMessageBuffer
		err  error
	}
	resCh := make(chan fetchResult, 1)
	go func() {
		msgs, err := fetchCmd.Collect()
		resCh <- fetchResult{msgs, err}
	}()

	<-literalOpen
	// The client is now inside the response with a literal open. Everything
	// recorded from here until we let the literal finish must keep the long
	// literal deadline.
	mark := recorder.countDeadlines()

	// Send a command from another goroutine, exactly the pipelining case. The
	// write blocks until the server reads it, which is after the literal
	// completes, so this goroutine is deliberately not waited on here.
	go client.Noop()
	time.Sleep(100 * time.Millisecond)

	for _, d := range recorder.deadlinesSince(mark) {
		if remaining := time.Until(d); remaining < 2*time.Minute {
			t.Errorf("a %v read deadline was set while a literal was open: a large FETCH body would be truncated", remaining.Round(time.Second))
		}
	}

	close(finishLiteral)

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Fetch().Collect() = %v", res.err)
	}
	if len(res.msgs) != 1 {
		t.Fatalf("len(msgs) = %v, want 1", len(res.msgs))
	}
	for _, buf := range res.msgs[0].BodySection {
		if string(buf.Bytes) != literal {
			t.Errorf("body = %q, want %q", buf.Bytes, literal)
		}
	}
}
