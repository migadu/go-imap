package imapserver_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const storeTestMessage = "From: <test@example.com>\r\n" +
	"To: <test@example.com>\r\n" +
	"Subject: store test\r\n" +
	"Date: Wed, 11 May 2016 14:31:26 -0700\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"Hi\r\n"

// storeOptionsSession wraps a real in-memory session and records the
// StoreOptions the server parser handed to the backend, so a test can tell an
// absent UNCHANGEDSINCE modifier apart from an explicit "UNCHANGEDSINCE 0".
type storeOptionsSession struct {
	imapserver.Session

	mutex sync.Mutex
	got   *imap.StoreOptions
}

func (s *storeOptionsSession) Store(ctx context.Context, w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	s.mutex.Lock()
	if options != nil {
		captured := *options
		s.got = &captured
	} else {
		s.got = nil
	}
	s.mutex.Unlock()
	return s.Session.Store(ctx, w, numSet, flags, options)
}

func (s *storeOptionsSession) storeOptions() *imap.StoreOptions {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.got
}

// storeTestConn drives a server over the raw wire so a test can assert on the
// exact tagged completion line (which is where MODIFIED is carried).
type storeTestConn struct {
	t       *testing.T
	conn    net.Conn
	br      *bufio.Reader
	session *storeOptionsSession
	srvConn *imapserver.Conn
}

func (c *storeTestConn) send(s string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(s + "\r\n")); err != nil {
		c.t.Fatalf("write %q: %v", s, err)
	}
}

func (c *storeTestConn) readUntilTag(tag string) string {
	c.t.Helper()
	var buf bytes.Buffer
	for {
		line, err := c.br.ReadString('\n')
		buf.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			return buf.String()
		}
		if err != nil {
			c.t.Fatalf("reading response for tag %q: %v (got %q)", tag, err, buf.String())
		}
	}
}

// containsFlag reports whether resp mentions the given flag. Flag names are
// compared case-insensitively: the server canonicalizes them on the wire.
func containsFlag(resp string, flag imap.Flag) bool {
	return strings.Contains(strings.ToLower(resp), strings.ToLower(string(flag)))
}

// do sends a command and returns the full response up to and including the
// tagged completion line.
func (c *storeTestConn) do(tag, cmd string) string {
	c.t.Helper()
	c.send(tag + " " + cmd)
	return c.readUntilTag(tag)
}

func (c *storeTestConn) appendMessage(tag string) {
	c.t.Helper()
	c.send(fmt.Sprintf("%v APPEND INBOX {%v}", tag, len(storeTestMessage)))
	line, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatalf("reading APPEND continuation request: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		c.t.Fatalf("APPEND continuation = %q, want a continuation request", line)
	}
	if _, err := c.conn.Write([]byte(storeTestMessage + "\r\n")); err != nil {
		c.t.Fatalf("writing APPEND literal: %v", err)
	}
	if resp := c.readUntilTag(tag); !strings.Contains(resp, tag+" OK") {
		c.t.Fatalf("APPEND failed: %q", resp)
	}
}

// newStoreTestConn brings up a server backed by imapmemserver, logs in, appends
// numMessages messages to INBOX and selects it.
func newStoreTestConn(t *testing.T, caps imap.CapSet, numMessages int) *storeTestConn {
	t.Helper()

	const username, password = "user", "pass"

	memUser := imapmemserver.NewUser(username, password)
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	var (
		mutex   sync.Mutex
		session *storeOptionsSession
		srvConn *imapserver.Conn
	)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			s := &storeOptionsSession{Session: memServer.NewSession()}
			mutex.Lock()
			session, srvConn = s, conn
			mutex.Unlock()
			return s, nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("read greeting: %v", err)
	}

	// The greeting has been read, so NewSession has already run.
	mutex.Lock()
	c.session, c.srvConn = session, srvConn
	mutex.Unlock()
	if c.session == nil || c.srvConn == nil {
		t.Fatal("server did not report the new session")
	}

	if resp := c.do("login", "LOGIN "+username+" "+password); !strings.Contains(resp, "login OK") {
		t.Fatalf("LOGIN failed: %q", resp)
	}
	for i := 0; i < numMessages; i++ {
		c.appendMessage(fmt.Sprintf("append%v", i))
	}
	if resp := c.do("select", "SELECT INBOX"); !strings.Contains(resp, "select OK") {
		t.Fatalf("SELECT failed: %q", resp)
	}
	return c
}

// TestStoreUnchangedSincePresence pins the distinction RFC 7162 §3.1.3.1 draws
// between an absent UNCHANGEDSINCE modifier (unconditional store) and an
// explicit "UNCHANGEDSINCE 0" (the always-fail probe). Both reach the backend
// with UnchangedSince == 0, so only StoreOptions.UnchangedSinceSet separates
// them.
func TestStoreUnchangedSincePresence(t *testing.T) {
	condStoreCaps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}}

	tests := []struct {
		name    string
		caps    imap.CapSet
		cmd     string
		want    imap.StoreOptions
		wantCS  bool // CONDSTORE became enabled on the connection
		checkCS bool
	}{
		{
			name:    "modifier absent",
			caps:    condStoreCaps,
			cmd:     `UID STORE 1 +FLAGS (\Seen)`,
			want:    imap.StoreOptions{UnchangedSince: 0, UnchangedSinceSet: false},
			wantCS:  false,
			checkCS: true,
		},
		{
			name:    "explicit zero",
			caps:    condStoreCaps,
			cmd:     `UID STORE 1 (UNCHANGEDSINCE 0) +FLAGS (\Seen)`,
			want:    imap.StoreOptions{UnchangedSince: 0, UnchangedSinceSet: true},
			wantCS:  true,
			checkCS: true,
		},
		{
			name:    "explicit non-zero",
			caps:    condStoreCaps,
			cmd:     `UID STORE 1 (UNCHANGEDSINCE 5) +FLAGS (\Seen)`,
			want:    imap.StoreOptions{UnchangedSince: 5, UnchangedSinceSet: true},
			wantCS:  true,
			checkCS: true,
		},
		{
			// Without CONDSTORE the modifier is ignored, which must clear both
			// the value and its presence flag: leaving presence set would turn
			// the command into an always-fail store.
			name: "capability absent clears presence",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}},
			cmd:  `UID STORE 1 (UNCHANGEDSINCE 0) +FLAGS (\Seen)`,
			want: imap.StoreOptions{UnchangedSince: 0, UnchangedSinceSet: false},
		},
		{
			name: "capability absent clears non-zero value",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}},
			cmd:  `UID STORE 1 (UNCHANGEDSINCE 5) +FLAGS (\Seen)`,
			want: imap.StoreOptions{UnchangedSince: 0, UnchangedSinceSet: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newStoreTestConn(t, tt.caps, 1)
			c.do("store", tt.cmd)

			got := c.session.storeOptions()
			if got == nil {
				t.Fatal("backend Store was not reached")
			}
			if *got != tt.want {
				t.Errorf("StoreOptions = %+v, want %+v", *got, tt.want)
			}

			// STORE ... (UNCHANGEDSINCE n) is a CONDSTORE-enabling command
			// (RFC 7162 §3.1) for every n, including 0.
			if tt.checkCS {
				if gotCS := c.srvConn.CondStoreEnabled(); gotCS != tt.wantCS {
					t.Errorf("CondStoreEnabled() = %v, want %v", gotCS, tt.wantCS)
				}
			}
		})
	}
}

// TestStoreUnchangedSinceZeroFailsAll checks the reference backend honors the
// always-fail semantics of "UNCHANGEDSINCE 0": every message that carries a
// modification sequence is left untouched and reported in a MODIFIED response
// code on the tagged completion line (RFC 7162 §3.1.3).
func TestStoreUnchangedSinceZeroFailsAll(t *testing.T) {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}}

	t.Run("sequence numbers", func(t *testing.T) {
		c := newStoreTestConn(t, caps, 3)

		resp := c.do("store", `STORE 1,3 (UNCHANGEDSINCE 0) +FLAGS (\Seen)`)
		if !strings.Contains(resp, "store OK [MODIFIED 1,3]") {
			t.Errorf("conditional STORE = %q, want a tagged OK carrying [MODIFIED 1,3]", resp)
		}
		if containsFlag(resp, imap.FlagSeen) {
			t.Errorf("conditional STORE reported a flag update, want none: %q", resp)
		}

		// Nothing was stored.
		if resp := c.do("fetch", "FETCH 1:3 (FLAGS)"); containsFlag(resp, imap.FlagSeen) {
			t.Errorf("FETCH after a failed conditional STORE = %q, want no \\Seen", resp)
		}
	})

	t.Run("uids", func(t *testing.T) {
		c := newStoreTestConn(t, caps, 3)

		resp := c.do("store", `UID STORE 1:* (UNCHANGEDSINCE 0) +FLAGS (\Seen)`)
		if !strings.Contains(resp, "store OK [MODIFIED 1:3]") {
			t.Errorf("conditional UID STORE = %q, want a tagged OK carrying [MODIFIED 1:3]", resp)
		}
	})

	t.Run("silent still reports MODIFIED", func(t *testing.T) {
		c := newStoreTestConn(t, caps, 2)

		resp := c.do("store", `STORE 1:2 (UNCHANGEDSINCE 0) +FLAGS.SILENT (\Seen)`)
		if !strings.Contains(resp, "store OK [MODIFIED 1:2]") {
			t.Errorf("conditional silent STORE = %q, want a tagged OK carrying [MODIFIED 1:2]", resp)
		}
	})

	t.Run("absent modifier stores unconditionally", func(t *testing.T) {
		c := newStoreTestConn(t, caps, 3)

		resp := c.do("store", `STORE 1:3 +FLAGS (\Seen)`)
		if strings.Contains(resp, "MODIFIED") {
			t.Errorf("unconditional STORE = %q, want no MODIFIED response code", resp)
		}
		if !strings.Contains(resp, "store OK") {
			t.Errorf("unconditional STORE = %q, want a tagged OK", resp)
		}
		if !containsFlag(resp, imap.FlagSeen) {
			t.Errorf("unconditional STORE = %q, want untagged FETCH responses carrying \\Seen", resp)
		}
	})

	t.Run("high modseq stores unconditionally", func(t *testing.T) {
		c := newStoreTestConn(t, caps, 3)

		// Every message's modseq is far below this, so none fails.
		resp := c.do("store", `STORE 1:3 (UNCHANGEDSINCE 1000000) +FLAGS (\Seen)`)
		if strings.Contains(resp, "MODIFIED") {
			t.Errorf("satisfied conditional STORE = %q, want no MODIFIED response code", resp)
		}
		if !containsFlag(resp, imap.FlagSeen) {
			t.Errorf("satisfied conditional STORE = %q, want untagged FETCH responses carrying \\Seen", resp)
		}
	})

	t.Run("partial failure", func(t *testing.T) {
		c := newStoreTestConn(t, caps, 3)

		// Bump message 3's modseq above the others, then store with an
		// UNCHANGEDSINCE that only it exceeds.
		if resp := c.do("bump", `STORE 3 +FLAGS (\Flagged)`); !strings.Contains(resp, "bump OK") {
			t.Fatalf("priming STORE failed: %q", resp)
		}
		resp := c.do("store", `STORE 1:3 (UNCHANGEDSINCE 4) +FLAGS (\Seen)`)
		if !strings.Contains(resp, "store OK [MODIFIED 3]") {
			t.Errorf("partially failing STORE = %q, want a tagged OK carrying [MODIFIED 3]", resp)
		}
		if !containsFlag(resp, imap.FlagSeen) {
			t.Errorf("partially failing STORE = %q, want untagged FETCH responses for the stored messages", resp)
		}
	})
}
