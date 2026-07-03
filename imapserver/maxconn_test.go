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

// L6: with MaxConnections set, a connection beyond the limit is refused with a
// BYE rather than served.
func TestMaxConnections(t *testing.T) {
	memServer := imapmemserver.New()
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth:   true,
		Caps:           imap.CapSet{imap.CapIMAP4rev2: {}},
		MaxConnections: 1,
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go server.Serve(ln)
	defer server.Close()

	readLine := func(conn net.Conn) string {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		return line
	}

	// First connection takes the only slot and is served (gets the greeting).
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() first = %v", err)
	}
	defer c1.Close()
	if got := readLine(c1); !strings.HasPrefix(got, "* OK") {
		t.Fatalf("first connection greeting = %q, want a * OK greeting", got)
	}

	// Second connection is over the limit and must be refused with BYE.
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() second = %v", err)
	}
	defer c2.Close()
	if got := readLine(c2); !strings.Contains(got, "BYE Too many connections") {
		t.Fatalf("second connection response = %q, want a BYE refusal", got)
	}
}
