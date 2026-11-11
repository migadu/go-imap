package imapclient

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/migadu/go-imap"
)

type cancellationWriteCounter struct {
	net.Conn
	writes atomic.Int64
}

func (c *cancellationWriteCounter) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

func TestCommandsAfterCancellation(t *testing.T) {
	for _, caps := range []string{"IMAP4rev1", "IMAP4rev1 LITERAL-"} {
		t.Run(caps, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			counter := &cancellationWriteCounter{Conn: clientConn}
			go io.WriteString(serverConn, "* PREAUTH [CAPABILITY "+caps+"] ready\r\n")
			client := New(counter, nil)
			if err := client.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				check := func(name string, err error) {
					if err == nil {
						t.Errorf("%s after cancellation returned no error", name)
					}
				}
				check("NOOP", client.Noop().Wait())
				check("LOGIN with literal", client.Login("user\nname", "password").Wait())
				appendCmd := client.Append("INBOX", 1, nil)
				_, err := appendCmd.Write([]byte("x"))
				check("APPEND Write", err)
				check("APPEND Close", appendCmd.Close())
				_, err = appendCmd.Wait()
				check("APPEND Wait", err)
				catenateCmd := client.AppendCatenate("INBOX", nil)
				text := catenateCmd.Text(1)
				_, err = text.Write([]byte("x"))
				check("CATENATE Write", err)
				check("CATENATE Text Close", text.Close())
				catenateCmd.Close()
				_, err = catenateCmd.Wait()
				check("CATENATE Wait", err)
				_, err = client.List("", "*", nil).Collect()
				check("LIST", err)
				_, err = client.Fetch(imap.SeqSetNum(1), nil).Collect()
				check("FETCH", err)
				_, err = client.Expunge().Collect()
				check("EXPUNGE", err)
				check("AUTHENTICATE", client.Authenticate(sasl.NewPlainClient("", "user", "pass")))
				_, err = client.Idle()
				check("IDLE", err)
				check("STARTTLS", client.startTLS(&tls.Config{}))
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("a command after cancellation blocked")
			}
			client.mutex.Lock()
			pending, continuations := len(client.pendingCmds), len(client.contReqs)
			client.mutex.Unlock()
			if pending != 0 || continuations != 0 || counter.writes.Load() != 0 {
				t.Errorf("rejected commands left %d pending, %d continuations, %d transport writes", pending, continuations, counter.writes.Load())
			}
		})
	}
}

func TestCancellationReleasesQueuedEncoderCommands(t *testing.T) {
	for _, idle := range []bool{false, true} {
		name := "APPEND owns encoder"
		if idle {
			name = "IDLE owns encoder"
		}
		t.Run(name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			go func() {
				io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 IDLE LITERAL-] ready\r\n")
				br := bufio.NewReader(serverConn)
				if idle {
					if _, err := br.ReadString('\n'); err == nil {
						io.WriteString(serverConn, "+ idling\r\n")
					}
				}
				io.Copy(io.Discard, br)
			}()
			client := New(clientConn, &Options{AutoIdle: time.Hour})
			defer client.Close()
			var idleCmd *IdleCommand
			var appendCmd *AppendCommand
			if idle {
				var err error
				idleCmd, err = client.Idle()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				appendCmd = client.Append("INBOX", 1, nil)
				defer appendCmd.Close()
			}
			queued := make(chan error, 1)
			go func() { queued <- client.Noop().Wait() }()
			// NOOP takes autoIdleMutex before waiting for the held encoder.
			deadline := time.Now().Add(time.Second)
			for {
				if !client.autoIdleMutex.TryLock() {
					break
				}
				client.autoIdleMutex.Unlock()
				if time.Now().After(deadline) {
					t.Fatal("NOOP did not queue behind the encoder")
				}
				time.Sleep(time.Millisecond)
			}
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close waited for an encoder held by the caller")
			}
			select {
			case err := <-queued:
				if err == nil {
					t.Error("queued NOOP succeeded after cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("queued NOOP did not observe cancellation")
			}
			if idleCmd != nil {
				if err := idleCmd.Wait(); err == nil {
					t.Error("IDLE Wait returned no cancellation error")
				}
			}
		})
	}
}

type cancellationPanicReader struct {
	net.Conn
	panicNext bool // only the reader goroutine accesses this field
}

func (c *cancellationPanicReader) Read(p []byte) (int, error) {
	if c.panicNext {
		panic("tagged response panic sentinel")
	}
	n, err := c.Conn.Read(p)
	if strings.Contains(string(p[:n]), "T1 OK [") {
		c.panicNext = true
	}
	return n, err
}

func TestTaggedResponsePanicCompletesRemovedCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
		br := bufio.NewReader(serverConn)
		if _, err := br.ReadString('\n'); err == nil {
			io.WriteString(serverConn, "T1 OK [COPYUID 1 2 ")
		}
	}()
	client := New(&cancellationPanicReader{Conn: clientConn}, nil)
	defer client.Close()
	cmd := client.Noop()
	select {
	case <-client.Closed():
	case <-time.After(time.Second):
		t.Fatal("reader did not terminate after panic")
	}
	select {
	case <-cmd.done:
		if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "tagged response panic sentinel") {
			t.Errorf("command error = %v", err)
		}
	default:
		t.Fatal("panic orphaned the command removed from the pending queue")
	}
}
