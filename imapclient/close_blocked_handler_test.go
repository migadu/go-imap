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

// Response processing normally waits for a Mailbox handler. A command awaited
// inside it cannot complete until cancellation releases the reader (#135).
func TestCloseReleasesHandlerWaitingForCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	notify := make(chan struct{})
	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		io.WriteString(serverConn, "* 3 EXISTS\r\n"+strings.Fields(line)[0]+" OK selected\r\n")
		<-notify
		io.WriteString(serverConn, "* 4 EXISTS\r\n")
		// Consume the callback's NOOP but deliberately leave it unanswered.
		br.ReadString('\n')
	}()

	var client *imapclient.Client
	waiting := make(chan struct{})
	handlerErr := make(chan error, 1)
	client = imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(*imapclient.UnilateralDataMailbox) {
				cmd := client.Noop()
				close(waiting)
				handlerErr <- cmd.Wait()
			},
		},
	})
	defer client.Close()
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	close(notify)
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not send its command")
	}

	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the handler which was waiting for a command")
	}
	select {
	case err := <-handlerErr:
		if err == nil {
			t.Fatal("handler's command succeeded after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler's command did not complete after Close")
	}
}
