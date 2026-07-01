package imapclient_test

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// runStoreModified drives a conditional STORE against a minimal scripted server
// that replies with the given tagged completion line (without the leading tag
// or trailing CRLF), e.g. "OK [MODIFIED 2] Conditional STORE completed". It
// returns the executed command and the error reported by Collect.
func runStoreModified(t *testing.T, numSet imap.NumSet, tagged string) (*imapclient.FetchCommand, error) {
	t.Helper()

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		r := bufio.NewReader(conn)
		if _, err := conn.Write([]byte("* OK [CAPABILITY IMAP4rev1 CONDSTORE] server ready\r\n")); err != nil {
			serverErr <- err
			return
		}

		// The client only issues the single conditional STORE command.
		line, err := r.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			serverErr <- err
			return
		}
		tag := fields[0]

		// Reply with only the tagged completion carrying MODIFIED: this mirrors
		// the case where every message failed the UNCHANGEDSINCE precondition,
		// so no FETCH data is emitted.
		if _, err := conn.Write([]byte(tag + " " + tagged + "\r\n")); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	client := imapclient.New(conn, nil)
	defer client.Close()

	cmd := client.Store(numSet, &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, &imap.StoreOptions{UnchangedSince: 100})
	_, collectErr := cmd.Collect()

	if err := <-serverErr; err != nil {
		t.Errorf("scripted server error: %v", err)
	}

	return cmd, collectErr
}

// TestStore_Modified verifies that the client surfaces the [MODIFIED <set>]
// response code from a conditional STORE (RFC 7162 §3.1.3) via
// FetchCommand.Modified, in the command's number space, on both OK and NO
// completions.
func TestStore_Modified(t *testing.T) {
	t.Run("SeqOK", func(t *testing.T) {
		cmd, err := runStoreModified(t, imap.SeqSetNum(1, 2, 3),
			"OK [MODIFIED 2,3] Conditional STORE completed")
		if err != nil {
			t.Errorf("Collect() = %v, want nil for OK [MODIFIED]", err)
		}
		modified := cmd.Modified()
		if _, ok := modified.(imap.SeqSet); !ok {
			t.Fatalf("Modified() type = %T, want imap.SeqSet", modified)
		}
		if got, want := modified.String(), imap.SeqSetNum(2, 3).String(); got != want {
			t.Errorf("Modified() = %v, want %v", got, want)
		}
	})

	t.Run("UIDOK", func(t *testing.T) {
		cmd, err := runStoreModified(t, imap.UIDSetNum(10, 11, 12),
			"OK [MODIFIED 11,12] Conditional STORE completed")
		if err != nil {
			t.Errorf("Collect() = %v, want nil for OK [MODIFIED]", err)
		}
		modified := cmd.Modified()
		if _, ok := modified.(imap.UIDSet); !ok {
			t.Fatalf("Modified() type = %T, want imap.UIDSet", modified)
		}
		if got, want := modified.String(), imap.UIDSetNum(11, 12).String(); got != want {
			t.Errorf("Modified() = %v, want %v", got, want)
		}
	})

	t.Run("NoModified", func(t *testing.T) {
		// A regular (non-conditional-failure) STORE completion must leave
		// Modified nil.
		cmd, err := runStoreModified(t, imap.SeqSetNum(1),
			"OK Conditional STORE completed")
		if err != nil {
			t.Errorf("Collect() = %v, want nil", err)
		}
		if modified := cmd.Modified(); modified != nil {
			t.Errorf("Modified() = %v, want nil", modified)
		}
	})

	t.Run("SeqNO", func(t *testing.T) {
		// Expunged-message case: NO [MODIFIED] — Modified is populated and the
		// command still fails.
		cmd, err := runStoreModified(t, imap.SeqSetNum(1, 2),
			"NO [MODIFIED 1,2] Some of the messages no longer exist")
		if err == nil {
			t.Errorf("Collect() = nil, want *imap.Error for NO [MODIFIED]")
		} else if imapErr, ok := err.(*imap.Error); !ok {
			t.Errorf("Collect() error type = %T, want *imap.Error", err)
		} else if imapErr.Code != "MODIFIED" {
			t.Errorf("imap.Error.Code = %q, want %q", imapErr.Code, "MODIFIED")
		}
		modified := cmd.Modified()
		if _, ok := modified.(imap.SeqSet); !ok {
			t.Fatalf("Modified() type = %T, want imap.SeqSet", modified)
		}
		if got, want := modified.String(), imap.SeqSetNum(1, 2).String(); got != want {
			t.Errorf("Modified() = %v, want %v", got, want)
		}
	})

	t.Run("UIDNO", func(t *testing.T) {
		cmd, err := runStoreModified(t, imap.UIDSetNum(20, 21),
			"NO [MODIFIED 20,21] Some of the messages no longer exist")
		if err == nil {
			t.Errorf("Collect() = nil, want *imap.Error for NO [MODIFIED]")
		}
		modified := cmd.Modified()
		if _, ok := modified.(imap.UIDSet); !ok {
			t.Fatalf("Modified() type = %T, want imap.UIDSet", modified)
		}
		if got, want := modified.String(), imap.UIDSetNum(20, 21).String(); got != want {
			t.Errorf("Modified() = %v, want %v", got, want)
		}
	})
}
