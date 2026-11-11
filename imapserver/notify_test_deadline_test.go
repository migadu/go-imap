package imapserver_test

import (
	"bufio"
	"io"
	"net"
	"strings"
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

// IDLE can outlive earlier test commands. Its raw request and DONE
// continuation must both work after the preceding write deadline expires.
func TestNotifyTestConnIdleWriteDeadline(t *testing.T) {
	addr, _, closer := newNotifyTestServer(t)
	defer closer()
	c := dialNotifyTest(t, addr)
	defer c.close()
	c.login(t)
	c.cmd("CREATE INBOX")
	c.cmd("SELECT INBOX")
	expire := func() {
		t.Helper()
		if err := c.conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	c.write("T90 IDLE\r\n")
	if line, err := c.readLine(5 * time.Second); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE continuation = %q, %v", line, err)
	}
	expire()
	c.write("DONE\r\n")
	if line, err := c.readLine(5 * time.Second); err != nil || !strings.HasPrefix(line, "T90 OK ") {
		t.Fatalf("IDLE completion = %q, %v", line, err)
	}
}

// Refreshing a write must not reopen a watcher's already-expired read window.
func TestNotifyTestConnWritePreservesReadDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := &notifyTestConn{t: t, conn: clientConn, br: bufio.NewReader(clientConn), timeout: notifyTestTimeout}
	go bufio.NewReader(serverConn).ReadString('\n')
	if err := clientConn.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	c.write("T1 NOOP\r\n")
	result := make(chan error, 1)
	go func() { _, err := c.br.ReadString('\n'); result <- err }()
	select {
	case err := <-result:
		timeout, ok := err.(net.Error)
		if !ok || !timeout.Timeout() {
			t.Fatalf("expired watcher read = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write refreshed the watcher's read deadline")
	}
}
