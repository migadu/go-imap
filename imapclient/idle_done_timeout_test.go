package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap/imapclient"
)

// RFC 2177 §3: "The IDLE command is terminated by the receipt of a "DONE"
// continuation from the client", and the server then completes it with the
// tagged response. Silence is what IDLE is for; silence after DONE is a
// server that owes a response, to be noticed within Options.ResponseTimeout
// like any other.
//
// The scenario is the one of emersion/go-imap#772: the server acknowledges
// IDLE, then the connection goes half-open. Wait used to block until the
// 30-minute idle timeout. Issue #131.
func TestIdleDoneBoundByResponseTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 IDLE] ready\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if fields := strings.Fields(line); len(fields) > 1 && fields[1] == "IDLE" {
				io.WriteString(serverConn, "+ idling\r\n")
			}
			// DONE, and whatever follows: no answer.
		}
	}()

	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 300 * time.Millisecond})
	defer client.Close()

	idleCmd, err := client.Idle()
	if err != nil {
		t.Fatalf("Idle() = %v", err)
	}
	// Let the reader park on the idle timeout, as it does in a real IDLE.
	time.Sleep(100 * time.Millisecond)
	if err := idleCmd.Close(); err != nil {
		t.Fatalf("IdleCommand.Close() = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- idleCmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("IdleCommand.Wait() = nil, want an error: the server never answered DONE")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("IdleCommand.Wait() still blocked 5s after DONE with a 300ms ResponseTimeout")
	}
}

// An IDLE command is not running until its continuation arrives. In
// particular, a peer that advertises IDLE and then ignores the command must
// not hold the encoder lock for the 30-minute idle timeout.
func TestIdleContinuationBoundByResponseTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 IDLE] ready\r\n")
		// Accept the command but deliberately send no continuation.
		bufio.NewReader(serverConn).ReadString('\n')
	}()
	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 100 * time.Millisecond})
	defer client.Close()
	done := make(chan error, 1)
	go func() { _, err := client.Idle(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Idle succeeded without a continuation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Idle still waits for its continuation after ResponseTimeout")
	}
}
