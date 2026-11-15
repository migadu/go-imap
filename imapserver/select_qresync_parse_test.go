package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// TestSelectQResyncParams covers the optional parts of the QRESYNC select
// parameter, "[SP known-uids] [SP seq-match-data]" (RFC 7162 §3.2.5).
//
// Either may be absent. known-uids is a sequence set and seq-match-data is
// parenthesised, so a parser that reaches for the sequence set first has to be
// able to recover when it finds "(" instead. It could not: the attempt recorded
// a decode error, and the decoder stops reading input once one is recorded, so
// a client that sent seq-match-data without known-uids lost the remainder of
// its SELECT.
func TestSelectQResyncParams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
	}{
		{name: "neither", params: "(QRESYNC (1 1))"},
		{name: "known-uids only", params: "(QRESYNC (1 1 1:10))"},
		{name: "seq-match-data only", params: "(QRESYNC (1 1 (1:5 100:105)))"},
		{name: "both", params: "(QRESYNC (1 1 1:10 (1:5 100:105)))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newQResyncTestConn(t)

			rc.send("a2 ENABLE QRESYNC")
			resp := rc.readUntilTag("a2")
			if !strings.Contains(resp[len(resp)-1], "OK") {
				t.Fatalf("ENABLE QRESYNC = %q, want OK", resp[len(resp)-1])
			}

			rc.send("a3 SELECT INBOX " + tc.params)
			resp = rc.readUntilTag("a3")
			if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a3 OK") {
				t.Errorf("SELECT INBOX %v = %q, want OK:\n\t%s", tc.params, tagged, strings.Join(resp, "\n\t"))
			}
		})
	}
}

// TestUIDFetchTrailingVanished covers the non-standard
// "(CHANGEDSINCE n) VANISHED" hybrid the parser accepts after the modifier
// list, and the malformed trailing token that has to be refused instead.
// A modifier the server doesn't know, or one in the wrong place, is a client
// error: BAD, not NO [SERVERBUG].
func TestUIDFetchTrailingVanished(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "trailing VANISHED", command: "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1) VANISHED", want: "a3 OK"},
		{name: "trailing junk", command: "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1) (", want: "a3 BAD"},
		{name: "trailing unknown atom", command: "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1) FOO", want: "a3 BAD"},
		{name: "unknown modifier", command: "UID FETCH 1:* (FLAGS) (FOO 1)", want: "a3 BAD"},
		{name: "unknown bare modifier", command: "UID FETCH 1:* FLAGS FOO 1", want: "a3 BAD"},
		{name: "VANISHED without UID", command: "FETCH 1:* (FLAGS) (CHANGEDSINCE 1 VANISHED)", want: "a3 BAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newQResyncTestConn(t)

			rc.send("a2 ENABLE QRESYNC")
			rc.readUntilTag("a2")
			rc.send("x2 SELECT INBOX")
			rc.readUntilTag("x2")

			rc.send("a3 " + tc.command)
			resp := rc.readUntilTag("a3")
			if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, tc.want) {
				t.Errorf("%v = %q, want %v:\n\t%s", tc.command, tagged, tc.want, strings.Join(resp, "\n\t"))
			}
		})
	}
}

// newQResyncTestConn returns a logged-in connection to a server advertising
// CONDSTORE and QRESYNC.
func newQResyncTestConn(t *testing.T) *roConn {
	t.Helper()

	const (
		username = "user"
		password = "pass"
	)

	memUser := imapmemserver.NewUser(username, password)
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create(INBOX): %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
			imap.CapCondStore: {},
			imap.CapQResync:   {},
		},
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	rc := &roConn{t: t, c: conn, br: bufio.NewReader(conn)}
	if _, err := rc.br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("read greeting: %v", err)
	}
	rc.send("x LOGIN " + username + " " + password)
	rc.readUntilTag("x")

	return rc
}

// "$" (RFC 5182) names the last SEARCH result, which is a set of UIDs, so it
// cannot stand in for the known-sequence-set half of seq-match-data. The server
// has to refuse it and stay on the connection.
func TestSelectQResyncSeqMatchSearchRes(t *testing.T) {
	rc := newQResyncTestConn(t)

	rc.send("a2 ENABLE QRESYNC")
	rc.readUntilTag("a2")

	rc.send("a3 SELECT INBOX (QRESYNC (1 1 ($ 100:105)))")
	resp := rc.readUntilTag("a3")
	if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a3 BAD") {
		t.Errorf("SELECT with \"$\" in seq-match-data = %q, want BAD:\n\t%s", tagged, strings.Join(resp, "\n\t"))
	}

	rc.send("a4 NOOP")
	resp = rc.readUntilTag("a4")
	if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a4 OK") {
		t.Errorf("NOOP after the refused SELECT = %q, want OK", tagged)
	}
}

// TestSelectQResyncWithoutEnable covers RFC 7162 §3.2.5: "A server MUST
// respond with a tagged BAD response if the Quick Resynchronization parameter
// to the SELECT/EXAMINE command is specified and the client hasn't issued
// "ENABLE QRESYNC" in the current connection". The parser returned from the
// parameter without reading its arguments instead, so a bare "(QRESYNC)" was
// accepted and the mailbox selected, and a well-formed parameter was refused
// only because the parser tripped over the argument list it had left behind.
func TestSelectQResyncWithoutEnable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enable  string
		command string
	}{
		{name: "SELECT", command: "SELECT INBOX (QRESYNC (1 1))"},
		{name: "SELECT with known-uids and seq-match-data", command: "SELECT INBOX (QRESYNC (1 1 1:10 (1:5 100:105)))"},
		{name: "EXAMINE", command: "EXAMINE INBOX (QRESYNC (1 1))"},
		{name: "bare QRESYNC", command: "SELECT INBOX (QRESYNC)"},
		{name: "bare QRESYNC before CONDSTORE", command: "SELECT INBOX (QRESYNC CONDSTORE)"},
		{name: "after ENABLE CONDSTORE", enable: "CONDSTORE", command: "SELECT INBOX (QRESYNC (1 1))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newQResyncTestConn(t)

			if tc.enable != "" {
				rc.send("a1 ENABLE " + tc.enable)
				rc.readUntilTag("a1")
			}

			rc.send("a2 " + tc.command)
			resp := rc.readUntilTag("a2")
			tagged := resp[len(resp)-1]
			if !strings.HasPrefix(tagged, "a2 BAD") || !strings.Contains(tagged, "ENABLE QRESYNC") {
				t.Errorf("%v without ENABLE QRESYNC = %q, want BAD naming ENABLE QRESYNC:\n\t%s", tc.command, tagged, strings.Join(resp, "\n\t"))
			}

			// The refused command must not have selected the mailbox, and the
			// rest of its line must have been discarded.
			rc.send("a3 CLOSE")
			resp = rc.readUntilTag("a3")
			if tagged := resp[len(resp)-1]; strings.HasPrefix(tagged, "a3 OK") {
				t.Errorf("CLOSE after the refused %v = %q, want it refused: no mailbox is selected", tc.command, tagged)
			}
			rc.send("a4 NOOP")
			resp = rc.readUntilTag("a4")
			if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a4 OK") {
				t.Errorf("NOOP after the refused %v = %q, want OK", tc.command, tagged)
			}
		})
	}
}

// With QRESYNC enabled, the parameter still needs its argument list
// (RFC 7162 §7: "QRESYNC" SP "(" uidvalidity SP mod-sequence-value ...).
func TestSelectQResyncWithoutArguments(t *testing.T) {
	rc := newQResyncTestConn(t)

	rc.send("a1 ENABLE QRESYNC")
	rc.readUntilTag("a1")

	rc.send("a2 SELECT INBOX (QRESYNC)")
	resp := rc.readUntilTag("a2")
	if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a2 BAD") {
		t.Errorf("SELECT INBOX (QRESYNC) = %q, want BAD:\n\t%s", tagged, strings.Join(resp, "\n\t"))
	}
}
