package imapclient_test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// When Dovecot exits early, over a bad configuration for instance, the client
// end of the harness must see the end of the stream. It used to stay open, and
// every test waited for a greeting until the test binary timed out.
//
// Upstream report: emersion/go-imap#614. Issue #137.
func TestDovecotHarnessEarlyExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script standing in for doveadm")
	}

	// A doveadm that fails the way a bad configuration makes it fail.
	binDir := t.TempDir()
	script := "#!/bin/sh\necho 'doveadm: Fatal: Error reading configuration' >&2\nexit 89\n"
	if err := os.WriteFile(filepath.Join(binDir, "doveadm"), []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write the stand-in doveadm: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	conn, server := newDovecotClientServerPair(t)
	defer conn.Close()

	readErr := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Errorf("read a byte from a Dovecot that has exited")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client end is still open 10s after doveadm exited")
	}

	closed := make(chan struct{})
	go func() {
		server.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("dovecotServer.Close() blocked after doveadm exited")
	}
}

// Closing the harness must release a child writing more than the stdout pipe
// can hold, even when the client stops reading in the middle of its reply.
func TestDovecotHarnessCloseWithUnreadOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script standing in for doveadm")
	}

	binDir := t.TempDir()
	pidFilename := filepath.Join(binDir, "pid")
	script := "#!/bin/sh\necho $$ > " + strconv.Quote(pidFilename) + "\n" +
		"dd if=/dev/zero bs=65536 count=32 2>/dev/null\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(binDir, "doveadm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	conn, server := newDovecotClientServerPair(t)
	defer conn.Close()
	closed := make(chan struct{})
	// A failed assertion must not leave the deliberately blocked child alive.
	t.Cleanup(func() {
		select {
		case <-closed:
			return
		default:
		}
		data, err := os.ReadFile(pidFilename)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			if process, err := os.FindProcess(pid); err == nil {
				process.Kill()
			}
		}
	})

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatalf("read first byte of child's reply: %v", err)
	}
	go func() {
		server.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("dovecotServer.Close() blocked on unread stdout")
	}
}

// The ordinary life of the harness, with a stand-in that greets and then reads
// its input to the end, as Dovecot does: the greeting arrives, and Close ends
// the process by closing its input.
func TestDovecotHarnessCloseEndsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script standing in for doveadm")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '* PREAUTH ready\\r\\n'\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(binDir, "doveadm"), []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write the stand-in doveadm: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	conn, server := newDovecotClientServerPair(t)
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	greeting := make([]byte, len("* PREAUTH ready\r\n"))
	if _, err := io.ReadFull(conn, greeting); err != nil || string(greeting) != "* PREAUTH ready\r\n" {
		t.Fatalf("read %q, %v; want the greeting", greeting, err)
	}

	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("dovecotServer.Close() = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dovecotServer.Close() blocked")
	}
}
