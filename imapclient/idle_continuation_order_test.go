package imapclient

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// An IDLE queued behind an APPEND must not claim that APPEND's continuation.
// The APPEND writer pauses while it owns the encoder, before it registers its
// literal, so the IDLE call can be made at that exact point.
func TestIdleQueuedBehindAppendContinuation(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	gate := &appendWriteGate{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 IDLE] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		io.WriteString(serverConn, "+ send literal\r\n")
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		io.WriteString(serverConn, tag+" OK appended\r\n")
		line, err = br.ReadString('\n')
		if err != nil {
			return
		}
		tag = strings.Fields(line)[0]
		io.WriteString(serverConn, "+ idling\r\n")
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		io.WriteString(serverConn, tag+" OK idle complete\r\n")
	}()
	client := New(clientConn, &Options{AutoIdle: time.Hour, ResponseTimeout: time.Second, DebugWriter: gate})
	defer client.Close()
	appendDone := make(chan error, 1)
	go func() {
		// A long quoted mailbox forces a buffered write before the literal header.
		cmd := client.Append(strings.Repeat("m", 4096), 1, nil)
		_, writeErr := cmd.Write([]byte("x"))
		closeErr := cmd.Close()
		_, err := cmd.Wait()
		if err == nil {
			err = writeErr
		}
		if err == nil {
			err = closeErr
		}
		appendDone <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("APPEND did not reach write gate")
	}
	idleDone := make(chan *IdleCommand, 1)
	idleErr := make(chan error, 1)
	go func() {
		cmd, err := client.Idle()
		if err != nil {
			idleErr <- err
			return
		}
		idleDone <- cmd
	}()
	// beginCommand holds autoIdleMutex while it waits for APPEND's encoder.
	// Reaching that point proves the queued Idle call has started.
	deadline := time.Now().Add(time.Second)
	for {
		if !client.autoIdleMutex.TryLock() {
			break
		}
		client.autoIdleMutex.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("IDLE did not queue for the encoder")
		}
		time.Sleep(time.Millisecond)
	}
	release.Do(func() { close(gate.release) })
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatalf("APPEND: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued IDLE stole APPEND's continuation")
	}
	select {
	case cmd := <-idleDone:
		if err := cmd.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	case err := <-idleErr:
		t.Fatalf("IDLE: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("IDLE never received its own continuation")
	}
}

type appendWriteGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *appendWriteGate) Write(p []byte) (int, error) {
	if strings.Contains(string(p), " APPEND ") {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return len(p), nil
}
