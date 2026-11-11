package imapserver_test

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

// Watchers poll much sooner than the command timeout. A quiet poll must still
// time out, and the next read must use the restored longer deadline.
func TestNotifyTestConnReadLineDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := &notifyTestConn{t: t, conn: clientConn, br: bufio.NewReader(clientConn), timeout: notifyTestTimeout}
	read := make(chan error, 1)
	go func() { _, err := c.readLine(20 * time.Millisecond); read <- err }()
	select {
	case err := <-read:
		timeout, ok := err.(net.Error)
		if !ok || !timeout.Timeout() {
			t.Fatalf("quiet watcher read = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher read ignored its short timeout")
	}
	go io.WriteString(serverConn, "* OK watcher update\r\n")
	// Read directly so this assertion depends on readLine having restored the
	// deadline, rather than a later command or poll resetting it for us.
	line, err := c.br.ReadString('\n')
	if err != nil || line != "* OK watcher update\r\n" {
		t.Fatalf("read after quiet poll = %q, %v", line, err)
	}
}
