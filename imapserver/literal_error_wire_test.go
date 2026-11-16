package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// A literal the decoder refuses on sight -- a size beyond what the grammar's
// sanity bound allows -- is the client's syntax, so the answer is BAD rather
// than NO [SERVERBUG]. The announcement is rejected before any continuation
// request, so a synchronizing literal never has a payload to skip.
func TestOversizedLiteralAnnouncementIsBad(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{"beyond the sanity bound", `a2 SELECT {99999999999}`},
		{"does not fit in int64", `a2 SELECT {99999999999999999999999}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialParseServer(t, nil)
			fmt.Fprint(w.conn, tc.cmd+"\r\n")
			got := w.until("a2")
			if strings.Contains(got, "SERVERBUG") {
				t.Errorf("oversized literal answered %q; want BAD", got)
			}
			if !strings.HasPrefix(got, "a2 BAD") {
				t.Errorf("answer was %q; want BAD", got)
			}
			fmt.Fprint(w.conn, "a3 NOOP\r\n")
			if got := w.until("a3"); !strings.HasPrefix(got, "a3 OK") {
				t.Errorf("NOOP after refused synchronizing literal = %q; want OK", got)
			}
		})
	}
}

// The server's own size check still decides the response for a literal that is
// merely larger than it accepts: that stays NO [TOOBIG], the typed error the
// check returns, and is not folded into the syntax-error path.
func TestBufferedLiteralOverServerLimitIsTooBig(t *testing.T) {
	w := dialParseServer(t, nil)
	fmt.Fprint(w.conn, "a2 SELECT {5000}\r\n")
	got := w.until("a2")
	if !strings.HasPrefix(got, "a2 NO ") || !strings.Contains(got, "[TOOBIG]") {
		t.Errorf("answer was %q; want NO [TOOBIG]", got)
	}
}

// The non-synchronizing form of the same announcement has an unvalidated
// quantity of octets in flight behind it. The server cannot skip them, so it
// ends the connection with BYE rather than parse the next command from inside
// client data (RFC 9051 §2.2.1).
func TestOversizedNonSyncLiteralClosesConnection(t *testing.T) {
	w := dialParseServer(t, nil)
	fmt.Fprint(w.conn, "a2 SELECT {99999999999+}\r\n")
	for {
		_ = w.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		l, err := w.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("connection ended without a BYE: %v", err)
		}
		l = strings.TrimRight(l, "\r\n")
		if strings.HasPrefix(l, "* BYE") {
			return
		}
		if strings.HasPrefix(l, "a2 ") {
			t.Fatalf("got a tagged answer %q; want the connection closed with BYE", l)
		}
	}
}

func dialUnauthParseServer(t *testing.T, username, password string) *parseWire {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser(username, password)
	if err := u.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapLiteralPlus: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
		srv.Close()
	})
	go srv.Serve(ln) //nolint:errcheck

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	w := &parseWire{t: t, conn: c, rd: bufio.NewReader(c)}
	w.line()
	return w
}

// RFC 9051 §9 (literal = "{" number64 ["+"] "}" CRLF *CHAR8) and RFC 7888 §3
// require "}" to immediately precede CRLF. Followed by white space, "{N+}" or
// "{N}" is not a literal announcement: the line is answered with BAD (without
// a continuation request or consuming following octets as literal data), and
// the next line is read as the next command, matching DiscardLine (#267).
func TestLiteralAnnouncementTrailingWhitespaceIsBad(t *testing.T) {
	t.Run("LOGIN nonsync trailing space does not consume next line as literal", func(t *testing.T) {
		w := dialUnauthParseServer(t, "user1", "pass1")
		fmt.Fprint(w.conn, "a1 LOGIN {5+} \r\nuser1 pass1\r\n")
		first := w.line()
		if !strings.HasPrefix(first, "a1 BAD ") {
			t.Fatalf("LOGIN {5+}SP answered %q; want a1 BAD", first)
		}
	})

	for _, tc := range []struct {
		name string
		line string
	}{
		{"LOGIN nonsync one space", "a1 LOGIN {5+} \r\n"},
		{"LOGIN sync one space", "a1 LOGIN {5} \r\n"},
		{"LOGIN nonsync two spaces", "a1 LOGIN {5+}  \r\n"},
		{"LOGIN sync two spaces", "a1 LOGIN {5}  \r\n"},
		{"LOGIN nonsync tab", "a1 LOGIN {5+}\t\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialUnauthParseServer(t, "user1", "pass1")
			fmt.Fprint(w.conn, tc.line+"x2 NOOP\r\n")
			_ = w.conn.(*net.TCPConn).CloseWrite()
			first := w.line()
			if strings.HasPrefix(first, "+ ") {
				t.Fatalf("%q sent continuation %q; want a1 BAD", tc.line, first)
			}
			if !strings.HasPrefix(first, "a1 BAD ") {
				t.Fatalf("%q answered %q; want a1 BAD", tc.line, first)
			}
			second := w.until("x2")
			if !strings.HasPrefix(second, "x2 OK ") {
				t.Fatalf("next command after %q = %q; want x2 OK", tc.line, second)
			}
		})
	}

	for _, tc := range []struct {
		name string
		line string
	}{
		{"SELECT nonsync one space", "a2 SELECT {5+} \r\n"},
		{"SELECT sync one space", "a2 SELECT {5} \r\n"},
		{"SELECT nonsync two spaces", "a2 SELECT {5+}  \r\n"},
		{"APPEND nonsync one space", "a2 APPEND INBOX {5+} \r\n"},
		{"APPEND sync one space", "a2 APPEND INBOX {5} \r\n"},
		{"APPEND binary nonsync one space", "a2 APPEND INBOX ~{5+} \r\n"},
		{"unknown command nonsync one space", "a2 FOO {5+} \r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialParseServer(t, nil)
			fmt.Fprint(w.conn, tc.line+"x3 NOOP\r\n")
			_ = w.conn.(*net.TCPConn).CloseWrite()
			first := w.line()
			if strings.HasPrefix(first, "+ ") {
				t.Fatalf("%q sent continuation %q; want a2 BAD", tc.line, first)
			}
			if !strings.HasPrefix(first, "a2 BAD ") {
				t.Fatalf("%q answered %q; want a2 BAD", tc.line, first)
			}
			next := w.until("x3")
			if !strings.HasPrefix(next, "x3 OK ") {
				t.Fatalf("next command after %q = %q; want x3 OK", tc.line, next)
			}
		})
	}

	t.Run("valid CRLF and lone LF literals still accepted", func(t *testing.T) {
		for _, nl := range []string{"\r\n", "\n"} {
			w := dialUnauthParseServer(t, "user1", "pass1")
			fmt.Fprintf(w.conn, "a1 LOGIN {5+}%suser1 pass1\r\n", nl)
			if got := w.until("a1"); !strings.HasPrefix(got, "a1 OK ") {
				t.Fatalf("LOGIN {5+}%q = %q; want a1 OK", nl, got)
			}

			w2 := dialUnauthParseServer(t, "user1", "pass1")
			fmt.Fprintf(w2.conn, "a1 LOGIN {5}%s", nl)
			if cont := w2.line(); !strings.HasPrefix(cont, "+ ") {
				t.Fatalf("LOGIN {5}%q = %q; want continuation", nl, cont)
			}
			fmt.Fprint(w2.conn, "user1 pass1\r\n")
			if got := w2.until("a1"); !strings.HasPrefix(got, "a1 OK ") {
				t.Fatalf("LOGIN {5}%q completion = %q; want a1 OK", nl, got)
			}
		}
	})
}
