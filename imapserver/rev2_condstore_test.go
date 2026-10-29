package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// rev2Conn is a raw connection to an in-memory server with the given
// capabilities, logged in, with two messages in INBOX.
type rev2Conn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	tags int
}

func dialRev2(t *testing.T, caps imap.CapSet) *rev2Conn {
	t.Helper()
	user := imapmemserver.NewUser("ada", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := user.Append(context.Background(), "INBOX", strings.NewReader(storeTestMessage), &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	c := &rev2Conn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	c.do("LOGIN ada pass")
	return c
}

// do sends a command and returns the untagged lines of its answer, failing
// the test unless it is answered OK.
func (c *rev2Conn) do(command string) []string {
	c.t.Helper()
	c.tags++
	tag := fmt.Sprintf("T%d", c.tags)
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, command); err != nil {
		c.t.Fatalf("%s: %v", command, err)
	}
	var untagged []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("%s: %v", command, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, tag+" "); ok {
			if !strings.HasPrefix(rest, "OK") {
				c.t.Fatalf("%s: %s", command, line)
			}
			return untagged
		}
		untagged = append(untagged, line)
	}
}

func joined(lines []string) string { return strings.Join(lines, "\n") }

// IMAP4rev2 implies neither CONDSTORE nor QRESYNC (RFC 9051 Appendix F). A
// server configured with IMAP4rev2 alone advertises neither and enables
// neither; a client that enabled IMAP4rev2 alone gets EXPUNGE, not VANISHED,
// which is QRESYNC's (RFC 7162 §3.2.10), and no MODSEQ, which only a
// CONDSTORE-aware client gets (RFC 7162 §3.1).
func TestIMAP4rev2ImpliesNeitherCondStoreNorQResync(t *testing.T) {
	t.Run("server without them", func(t *testing.T) {
		c := dialRev2(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}})
		capability := joined(c.do("CAPABILITY"))
		for _, name := range []string{"CONDSTORE", "QRESYNC"} {
			if strings.Contains(capability, name) {
				t.Errorf("CAPABILITY lists %s: %q", name, capability)
			}
		}
		if enabled := joined(c.do("ENABLE IMAP4rev2 CONDSTORE QRESYNC")); enabled != "* ENABLED IMAP4rev2" {
			t.Errorf("ENABLE IMAP4rev2 CONDSTORE QRESYNC answered %q, want IMAP4rev2 alone", enabled)
		}
		c.do("SELECT INBOX")
		if store := joined(c.do(`STORE 1 +FLAGS (\Deleted)`)); strings.Contains(store, "MODSEQ") {
			t.Errorf("STORE answered with MODSEQ: %q", store)
		}
		if expunge := joined(c.do("EXPUNGE")); expunge != "* 1 EXPUNGE" {
			t.Errorf("EXPUNGE answered %q, want * 1 EXPUNGE", expunge)
		}
	})

	t.Run("client that enabled IMAP4rev2 alone", func(t *testing.T) {
		c := dialRev2(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapCondStore: {}, imap.CapQResync: {}})
		if enabled := joined(c.do("ENABLE IMAP4rev2")); enabled != "* ENABLED IMAP4rev2" {
			t.Errorf("ENABLE IMAP4rev2 answered %q", enabled)
		}
		c.do("SELECT INBOX")
		if store := joined(c.do(`STORE 1 +FLAGS (\Deleted)`)); strings.Contains(store, "MODSEQ") {
			t.Errorf("STORE answered with MODSEQ: %q", store)
		}
		if expunge := joined(c.do("EXPUNGE")); expunge != "* 1 EXPUNGE" {
			t.Errorf("EXPUNGE answered %q, want * 1 EXPUNGE", expunge)
		}
	})

	t.Run("client that enabled QRESYNC", func(t *testing.T) {
		c := dialRev2(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}, imap.CapCondStore: {}, imap.CapQResync: {}})
		if enabled := joined(c.do("ENABLE QRESYNC")); enabled != "* ENABLED QRESYNC" {
			t.Errorf("ENABLE QRESYNC answered %q", enabled)
		}
		c.do("SELECT INBOX")
		c.do(`STORE 1 +FLAGS.SILENT (\Deleted)`)
		if expunge := joined(c.do("EXPUNGE")); expunge != "* VANISHED 1" {
			t.Errorf("EXPUNGE answered %q, want * VANISHED 1", expunge)
		}
	})
}
