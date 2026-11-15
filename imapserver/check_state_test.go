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

// TestCheckOnlyInSelectedState checks that CHECK is refused outside the
// selected state, as the other selected-state commands are, while NOOP is
// served in every state.
//
// RFC 3501 §6.4: "In addition to the universal commands (CAPABILITY, NOOP,
// and LOGOUT), and the authenticated state commands (...), the following
// commands are valid in the selected state: CHECK, CLOSE, EXPUNGE, SEARCH,
// FETCH, STORE, COPY, and UID." §9 puts CHECK in command-select, "Valid only
// when in Selected state".
func TestCheckOnlyInSelectedState(t *testing.T) {
	memUser := imapmemserver.NewUser("user", "pass")
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create(INBOX): %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	rc := &rawConn{t: t, c: conn, br: bufio.NewReader(conn)}
	if _, err := rc.br.ReadString('\n'); err != nil {
		t.Fatalf("read greeting: %v", err)
	}

	steps := []struct {
		state, cmd, want string
	}{
		{"not authenticated", "CHECK", "BAD"},
		{"not authenticated", "NOOP", "OK"},
		{"not authenticated", "LOGIN user pass", "OK"},
		{"authenticated", "CHECK", "BAD"},
		{"authenticated", "NOOP", "OK"},
		{"authenticated", "SELECT INBOX", "OK"},
		{"selected", "CHECK", "OK"},
		{"selected", "NOOP", "OK"},
		{"selected", "CLOSE", "OK"},
		{"authenticated after CLOSE", "CHECK", "BAD"},
		{"authenticated after CLOSE", "EXAMINE INBOX", "OK"},
		{"selected read-only", "CHECK", "OK"},
	}
	for i, step := range steps {
		tag := "T" + string(rune('a'+i))
		rc.send(tag + " " + step.cmd)
		lines := rc.readUntilTag(tag)
		tagged := lines[len(lines)-1]
		if got := strings.Fields(tagged)[1]; got != step.want {
			t.Errorf("%v in the %v state: got %q, want %v", step.cmd, step.state, tagged, step.want)
		}
	}
}
