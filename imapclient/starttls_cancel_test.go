package imapclient

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestCloseReleasesStartTLSUpgradeWait(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	seen, send := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(send) })
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n")
		br := bufio.NewReader(serverConn)
		tag, _, err := readCancellationCommand(br)
		if err != nil {
			return
		}
		close(seen)
		<-send
		io.WriteString(serverConn, tag+" OK proceed\r")
		io.Copy(io.Discard, br)
	}()
	client := New(clientConn, nil)
	done := make(chan error, 1)
	go func() { done <- client.startTLS(&tls.Config{InsecureSkipVerify: true}) }()
	select {
	case <-seen:
	case <-time.After(cancellationTestTimeout):
		t.Fatal("server did not receive STARTTLS")
	}
	cmd := findPendingCmdByType[*startTLSCommand](client)
	if cmd == nil {
		t.Fatal("STARTTLS not pending")
	}
	release.Do(func() { close(send) })
	select {
	case <-cmd.done:
	case <-time.After(time.Second):
		t.Fatal("tagged OK not completed before CRLF")
	}
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("STARTTLS after cancellation = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		// The decoder has exited, so only this test owns the upgrade fields now.
		cmd.upgradeErr = net.ErrClosed
		close(cmd.upgradeDone)
		<-done
		t.Fatal("STARTTLS still waiting on upgradeDone after Close")
	}
}
