package imapclient_test

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func newDovecotClientServerPair(t *testing.T) (net.Conn, io.Closer) {
	tempDir := t.TempDir()

	cfgFilename := filepath.Join(tempDir, "dovecot.conf")
	cfg := `dovecot_config_version  = 2.4.0
dovecot_storage_version = 2.4.0

log_path      = "` + tempDir + `/dovecot.log"
ssl           = no
mail_home     = "` + tempDir + `/%{user}"
mail_driver   = maildir
mail_path     = "~/Mail"

namespace inbox {
	separator = /
	prefix =
	inbox = yes
}

mail_plugins {
	acl = yes
}

protocol imap {
	mail_plugins {
		imap_acl = yes
	}
}

acl_driver = vfile
`
	if err := os.WriteFile(cfgFilename, []byte(cfg), 0666); err != nil {
		t.Fatalf("failed to write Dovecot config: %v", err)
	}

	clientConn, serverConn := net.Pipe()

	cmd := exec.Command("doveadm", "-c", cfgFilename, "exec", "imap")
	cmd.Env = []string{"USER=" + testUsername, "PATH=" + os.Getenv("PATH")}
	cmd.Dir = tempDir
	cmd.Stderr = os.Stderr
	// The pipe is not handed to exec as Stdin and Stdout: Wait would then
	// wait for exec's own copy from it, which only ends when the pipe is
	// closed, and nothing would tell us that Dovecot has exited.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("failed to open Dovecot's stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("failed to open Dovecot's stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start Dovecot: %v", err)
	}

	srv := &dovecotServer{conn: serverConn, exited: make(chan struct{})}
	go func() {
		// Ends when the pipe is closed; Dovecot exits when its input ends.
		io.Copy(stdin, serverConn)
		stdin.Close()
	}()
	go func() {
		defer close(srv.exited)
		// Ends when Dovecot exits, whether it was asked to or not. Close
		// our end then, so that a client still waiting for it sees the end
		// of the stream instead of hanging.
		io.Copy(serverConn, stdout)
		serverConn.Close()
		// If the client closed its pipe while Dovecot was writing a reply,
		// the copy above ended before stdout did. Close the read end too:
		// otherwise Dovecot can remain blocked on a full stdout pipe and
		// never reach the EOF on stdin that asks it to exit.
		stdout.Close()
		srv.waitErr = cmd.Wait()
	}()

	return clientConn, srv
}

type dovecotServer struct {
	conn net.Conn
	// exited is closed once Dovecot has exited; waitErr is set by then.
	exited  chan struct{}
	waitErr error
}

func (srv *dovecotServer) Close() error {
	// Dovecot exits when its standard input ends. The pipe may be closed
	// already, if it exited by itself.
	srv.conn.Close()
	<-srv.exited
	return srv.waitErr
}
