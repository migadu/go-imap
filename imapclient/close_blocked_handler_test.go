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

// The Mailbox, Expunge and Vanished handlers run on the goroutine that reads
// responses, so a handler waiting for a command waits for a reply nobody
// reads. That is the caller's mistake (emersion/go-imap#668), but it must
// not be beyond repair: Close used to hang too, waiting for the read
// goroutine while the read goroutine waited for the command. Issue #135.
func TestCloseReleasesHandlerWaitingForCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n* 3 EXISTS\r\n")
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if fields := strings.Fields(line); len(fields) > 0 {
				io.WriteString(serverConn, fields[0]+" OK completed\r\n")
			}
		}
	}()

	var client *imapclient.Client
	ready := make(chan struct{})
	handlerErr := make(chan error, 1)
	client = imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(*imapclient.UnilateralDataMailbox) {
				<-ready
				handlerErr <- client.Noop().Wait()
			},
		},
	})
	close(ready)

	// Give the handler time to send its command and block.
	time.Sleep(200 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		client.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() still blocked after 5s: the handler waits for a command, Close for the handler")
	}
	select {
	case err := <-handlerErr:
		if err == nil {
			t.Errorf("the handler's Noop().Wait() = nil after Close, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler's Noop().Wait() did not return after Close")
	}
}
