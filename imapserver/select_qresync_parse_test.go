package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
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
func TestUIDFetchTrailingVanished(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "trailing VANISHED", command: "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1) VANISHED", want: "a3 OK"},
		{name: "trailing junk", command: "UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1) (", want: "a3 BAD"},
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
