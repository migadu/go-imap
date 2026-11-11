package imapclient

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestRunUnilateralHandlerWaitsUntilCompletion(t *testing.T) {
	c := &Client{closeCh: make(chan struct{})}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.runUnilateralHandler(func() {
			close(started)
			<-release
		})
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("handler handoff returned before the callback: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler completion did not release the decoder")
	}
}

func TestRunUnilateralHandlerCancelled(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := &Client{conn: clientConn, closeCh: make(chan struct{})}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.runUnilateralHandler(func() {
			defer close(finished)
			close(started)
			<-release
		})
	}()
	<-started
	c.closeWithError(net.ErrClosed)
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancellation returned no error")
		}
	case <-time.After(time.Second):
		t.Error("cancellation did not release the decoder")
	}
	close(release)
	<-finished

	// The same cancellation must stop subsequent callbacks before they start.
	if err := c.runUnilateralHandler(func() { t.Error("callback ran after cancellation") }); err == nil {
		t.Error("a cancelled callback handoff returned no error")
	}
}

func TestRunUnilateralHandlerPanicIsAnError(t *testing.T) {
	c := &Client{closeCh: make(chan struct{})}
	err := c.runUnilateralHandler(func() { panic("handler panic sentinel") })
	if err == nil || !strings.Contains(err.Error(), "handler panic sentinel") || !strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("panic error = %v, want panic value and stack", err)
	}
}
