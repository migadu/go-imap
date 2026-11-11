package imapclient_test

import (
	"net"
	"testing"
	"time"

	"github.com/migadu/go-imap/imapclient"
)

// RFC 9051 §7.1: the server opens the connection with a greeting, so a client
// that has just connected is owed a response, exactly as while a command is in
// flight. A server that accepts the connection and then says nothing must be
// noticed within Options.ResponseTimeout.
//
// The reader did arm that timeout for the greeting, and then replaced it with
// the 30-minute idle timeout before its first read, because no command was
// pending. Every command waits for the greeting before it is written, so all
// of them hung for half an hour too.
//
// Issue #129. The symptom is the one of emersion/go-imap#611 (reported
// against v1) and of the Dovecot test hang in emersion/go-imap#614.
func TestGreetingBoundByResponseTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close() // accepts, never writes

	client := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 200 * time.Millisecond})
	defer client.Close()

	for name, wait := range map[string]func() error{
		"WaitGreeting": client.WaitGreeting,
		"Noop":         func() error { return client.Noop().Wait() },
	} {
		done := make(chan error, 1)
		go func() { done <- wait() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s() = nil on a server that never greets", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s() still blocked 5s after a 200ms ResponseTimeout", name)
		}
	}
}
