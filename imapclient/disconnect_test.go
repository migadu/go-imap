package imapclient_test

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
)

// deadlinelessConn is a net.Conn whose deadline setters do nothing.
//
// That is the point of it. The client bounds a silent server with a read
// deadline, so on a normal connection a dropped peer would eventually surface
// as a timeout no matter how the EOF is handled. Ignoring the deadlines takes
// that safety net away and forces the tests below to exercise the path they are
// actually about: the read loop observing EOF and failing every pending command.
type deadlinelessConn struct {
	io.Reader
	io.Writer
	closer io.Closer
}

func (c deadlinelessConn) Close() error { return c.closer.Close() }

func (c deadlinelessConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

func (c deadlinelessConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

func (c deadlinelessConn) SetDeadline(time.Time) error      { return nil }
func (c deadlinelessConn) SetReadDeadline(time.Time) error  { return nil }
func (c deadlinelessConn) SetWriteDeadline(time.Time) error { return nil }

var _ net.Conn = deadlinelessConn{}

// deadlinelessPipe returns a connected client/server pair that ignores
// deadlines.
func deadlinelessPipe() (client, server deadlinelessConn) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	return deadlinelessConn{Reader: clientR, Writer: clientW, closer: clientW},
		deadlinelessConn{Reader: serverR, Writer: serverW, closer: serverW}
}

// serveThenDrop writes a greeting, waits for one command, then drops the
// connection without answering it.
func serveThenDrop(conn deadlinelessConn, bufSize int, delay time.Duration) {
	go func() {
		conn.Write([]byte("* OK [CAPABILITY IMAP4rev2] server ready\r\n"))
		buf := make([]byte, bufSize)
		conn.Read(buf)
		time.Sleep(delay)
		conn.Close()
	}()
}

// TestCommandWaitOnConnectionDrop checks that a command whose response will
// never arrive fails instead of blocking forever once the connection drops.
//
// A hang here is the worst failure mode this client has: the caller is given no
// error to react to and no timeout to bound the wait.
func TestCommandWaitOnConnectionDrop(t *testing.T) {
	clientConn, serverConn := deadlinelessPipe()

	client := imapclient.New(clientConn, nil)
	defer client.Close()

	serveThenDrop(serverConn, 1024, 50*time.Millisecond)

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	cmd := client.Noop()

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Wait() }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("Wait() = nil, want an error: the connection dropped before the response")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() hung after the connection dropped")
	}
}

// TestPendingCommandsUnblockOnConnectionDrop is the same property for more than
// one in-flight command: every pending command must be completed, not just the
// one at the head of the queue.
func TestPendingCommandsUnblockOnConnectionDrop(t *testing.T) {
	clientConn, serverConn := deadlinelessPipe()

	client := imapclient.New(clientConn, nil)
	defer client.Close()

	serveThenDrop(serverConn, 4096, 100*time.Millisecond)

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	cmds := []*imapclient.Command{client.Noop(), client.Noop(), client.Noop()}

	done := make(chan struct{})
	go func() {
		for _, cmd := range cmds {
			cmd.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pending commands hung after the connection dropped")
	}
}
