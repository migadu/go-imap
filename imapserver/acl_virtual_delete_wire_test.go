package imapserver_test

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// memServerACLRights runs one SETACL for bob on a fresh imapmemserver and
// returns the rights GETACL then reports for bob, so the virtual-right tests
// exercise the wire layer and the backend together.
func memServerACLRights(t *testing.T, setACLArgs string) string {
	t.Helper()
	memServer := imapmemserver.New()
	memServer.AddUser(imapmemserver.NewUser("owner", "pass"))

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
	})
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	readLine := func() string {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	cmd := func(tag, command string) (untagged []string, tagged string) {
		if _, err := conn.Write([]byte(tag + " " + command + "\r\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		for {
			line := readLine()
			if strings.HasPrefix(line, tag+" ") {
				return untagged, line
			}
			untagged = append(untagged, line)
		}
	}
	mustOK := func(tag, command string) []string {
		t.Helper()
		untagged, tagged := cmd(tag, command)
		if !strings.HasPrefix(tagged, tag+" OK") {
			t.Fatalf("%s: %s", command, tagged)
		}
		return untagged
	}

	readLine() // greeting
	mustOK("a1", "LOGIN owner pass")
	mustOK("a2", "CREATE Shared")
	mustOK("a3", "SETACL Shared bob "+setACLArgs)

	var row string
	for _, l := range mustOK("a4", "GETACL Shared") {
		if strings.HasPrefix(l, "* ACL ") {
			row = l
		}
	}
	if row == "" {
		t.Fatal("GETACL returned no untagged ACL row")
	}
	// The row is `* ACL Shared "owner" "<rights>" "bob" "<rights>"`; take bob's.
	i := strings.Index(row, `"bob" "`)
	if i < 0 {
		t.Fatalf("GETACL row has no entry for bob: %s", row)
	}
	rights := row[i+len(`"bob" "`):]
	return rights[:strings.IndexByte(rights, '"')]
}

// TestVirtualDeleteOverMemServer runs the virtual `d` end to end against
// imapmemserver, whose own SetACL also reads `d` as `t`+`e`: the two layers
// must agree, and the GETACL answer must show `t`, `e` and the compatibility
// `d` while never inventing an `x` the client did not ask for.
func TestVirtualDeleteOverMemServer(t *testing.T) {
	rights := memServerACLRights(t, "lrd")
	for _, want := range "lrted" {
		if !strings.ContainsRune(rights, want) {
			t.Errorf("bob's rights %q lack %q", rights, want)
		}
	}
	if strings.ContainsRune(rights, 'x') {
		t.Errorf("bob's rights %q carry `x`, which the virtual `d` must not confer", rights)
	}
	if strings.ContainsRune(rights, 'c') {
		t.Errorf("bob's rights %q carry `c`, although neither `k` nor `x` is held", rights)
	}
}

// TestVirtualCreateOverMemServer is the `c` counterpart: imapmemserver reads
// `c` as `k`+`x` like imapserver does, and GETACL reports `k`, `x` and the
// compatibility `c` - without a `d`, since no delete member is held.
func TestVirtualCreateOverMemServer(t *testing.T) {
	rights := memServerACLRights(t, "lrc")
	for _, want := range "lrkxc" {
		if !strings.ContainsRune(rights, want) {
			t.Errorf("bob's rights %q lack %q", rights, want)
		}
	}
	if strings.ContainsRune(rights, 'd') {
		t.Errorf("bob's rights %q carry `d`, although neither `t` nor `e` is held", rights)
	}

	// RFC 4314 §2.1.1's own example, `c` and `d` together: `x` is there, and
	// it came from `c`.
	rights = memServerACLRights(t, "lrswicd")
	for _, want := range "lrswikxtecd" {
		if !strings.ContainsRune(rights, want) {
			t.Errorf("bob's rights %q lack %q", rights, want)
		}
	}
}
