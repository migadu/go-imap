package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func TestWireLiteralValidationErrorDiscardsRemainderOfLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
	}{
		{
			name: "SELECT invalid UTF-7 nonsync literal discards trailing tokens",
			wire: "a2 SELECT {2+}\r\n&x hidden CREATE injected-mailbox\r\nreal NOOP\r\n",
		},
		{
			name: "LIST invalid UTF-7 nonsync literal discards trailing tokens",
			wire: "a2 LIST \"\" {2+}\r\n&x hidden CREATE injected-mailbox\r\nreal NOOP\r\n",
		},
		{
			name: "SEARCH SINCE invalid date nonsync literal discards trailing tokens",
			wire: "a2 SEARCH SINCE {3+}\r\nbad hidden CREATE injected-mailbox\r\nreal NOOP\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialFramingAuthServer(t)
			_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.WriteString(w.conn, tc.wire); err != nil {
				t.Fatal(err)
			}
			_ = w.conn.(*net.TCPConn).CloseWrite()
			raw, err := io.ReadAll(w.rd)
			if err != nil {
				t.Fatalf("read: %v (output %q)", err, raw)
			}
			out := string(raw)
			if strings.Contains(out, "hidden ") {
				t.Fatalf("remainder of rejected literal command line executed as a new command:\n%s", out)
			}
			if !strings.Contains(out, "a2 BAD ") || !strings.Contains(out, "real OK ") {
				t.Fatalf("expected a2 BAD followed by real OK, got:\n%s", out)
			}
		})
	}

	t.Run("RENAME invalid UTF-7 literal followed by unvalidated nonsync literal sends BYE", func(t *testing.T) {
		w := dialFramingAuthServer(t)
		_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
		payload := "hidden CREATE injected-mailbox\r\n"
		wire := fmt.Sprintf("a2 RENAME {2+}\r\n&x {%d+}\r\n%sreal NOOP\r\n", len(payload), payload)
		if _, err := io.WriteString(w.conn, wire); err != nil {
			t.Fatal(err)
		}
		_ = w.conn.(*net.TCPConn).CloseWrite()
		raw, err := io.ReadAll(w.rd)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, raw)
		}
		out := string(raw)
		if strings.Contains(out, "hidden ") {
			t.Fatalf("trailing nonsync literal payload executed as a command:\n%s", out)
		}
		if !strings.Contains(out, "* BYE ") {
			t.Fatalf("expected * BYE on unvalidated trailing nonsync literal, got:\n%s", out)
		}
	})
}

func TestWireQuotedCrossesCRLFAndExecutesLiteralPayload(t *testing.T) {
	t.Run("unclosed quote ending in nonsync literal with quote in payload", func(t *testing.T) {
		w := dialFramingAuthServer(t)
		_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
		payload := "x\"y\r\nhidden CREATE injected-mailbox\r\n"
		wire := fmt.Sprintf("a2 SELECT \"foo {%d+}\r\n%sreal NOOP\r\n", len(payload), payload)
		if _, err := io.WriteString(w.conn, wire); err != nil {
			t.Fatal(err)
		}
		_ = w.conn.(*net.TCPConn).CloseWrite()
		raw, err := io.ReadAll(w.rd)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, raw)
		}
		out := string(raw)
		if strings.Contains(out, "hidden ") {
			t.Fatalf("literal payload after unclosed quoted string executed as a command:\n%s", out)
		}
		if !strings.Contains(out, "* BYE ") {
			t.Fatalf("expected * BYE on unvalidated nonsync literal after unclosed quote, got:\n%s", out)
		}
	})

	t.Run("unclosed quote crosses into pipelined APPEND literal body", func(t *testing.T) {
		w := dialFramingAuthServer(t)
		_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
		body := "Subject: \"hi\"\r\nhidden CREATE injected-mailbox\r\n"
		wire := fmt.Sprintf("a2 SELECT \"unclosed\r\na3 APPEND INBOX {%d+}\r\n%s", len(body), body)
		if _, err := io.WriteString(w.conn, wire); err != nil {
			t.Fatal(err)
		}
		_ = w.conn.(*net.TCPConn).CloseWrite()
		raw, err := io.ReadAll(w.rd)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, raw)
		}
		out := string(raw)
		if strings.Contains(out, "hidden ") {
			t.Fatalf("email body line inside APPEND literal executed as an IMAP command:\n%s", out)
		}
	})
}

func TestWireAuthenticateContinuationNonSyncLiteralClosesWithBye(t *testing.T) {
	w := dialFramingUnauthServer(t, "user1", "pass1")
	_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(w.conn, "a1 AUTHENTICATE PLAIN\r\n"); err != nil {
		t.Fatal(err)
	}
	if cont := w.line(); !strings.HasPrefix(cont, "+") {
		t.Fatalf("continuation = %q, want '+'", cont)
	}
	payload := "hidden LOGIN user1 pass1\r\n"
	if _, err := fmt.Fprintf(w.conn, "{%d+}\r\n%s", len(payload), payload); err != nil {
		t.Fatal(err)
	}
	_ = w.conn.(*net.TCPConn).CloseWrite()
	raw, err := io.ReadAll(w.rd)
	if err != nil {
		t.Fatalf("read: %v (output %q)", err, raw)
	}
	out := string(raw)
	if strings.Contains(out, "hidden ") {
		t.Fatalf("AUTHENTICATE continuation nonsync literal payload executed as command:\n%s", out)
	}
	if !strings.Contains(out, "* BYE ") {
		t.Fatalf("expected * BYE on unvalidated nonsync literal in AUTHENTICATE continuation, got:\n%s", out)
	}
}

func TestWireIdleLineFramingBugs(t *testing.T) {
	t.Run("overlong IDLE line does not split at 4096 bytes and execute tail", func(t *testing.T) {
		w := dialFramingIdleServer(t, nil)
		_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
		fmt.Fprint(w.conn, "a2 IDLE\r\n")
		if cont := w.line(); !strings.HasPrefix(cont, "+ ") {
			t.Fatalf("IDLE continuation = %q, want '+ '", cont)
		}
		fmt.Fprint(w.conn, strings.Repeat("X", 4096)+"hidden CREATE injected-mailbox\r\n")
		_ = w.conn.(*net.TCPConn).CloseWrite()
		raw, err := io.ReadAll(w.rd)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, raw)
		}
		out := string(raw)
		if strings.Contains(out, "hidden ") {
			t.Fatalf("tail of overlong IDLE line executed as a new command:\n%s", out)
		}
	})

	t.Run("IDLE line ending in nonsync literal closes with BYE", func(t *testing.T) {
		w := dialFramingIdleServer(t, nil)
		_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
		fmt.Fprint(w.conn, "a2 IDLE\r\n")
		if cont := w.line(); !strings.HasPrefix(cont, "+ ") {
			t.Fatalf("IDLE continuation = %q, want '+ '", cont)
		}
		payload := "hidden CREATE injected-mailbox\r\n"
		fmt.Fprintf(w.conn, "NOTDONE {%d+}\r\n%s", len(payload), payload)
		_ = w.conn.(*net.TCPConn).CloseWrite()
		raw, err := io.ReadAll(w.rd)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, raw)
		}
		out := string(raw)
		if strings.Contains(out, "hidden ") {
			t.Fatalf("IDLE nonsync literal payload executed as a command:\n%s", out)
		}
		if !strings.Contains(out, "* BYE ") {
			t.Fatalf("expected * BYE on unvalidated nonsync literal during IDLE, got:\n%s", out)
		}
	})

	t.Run("IDLE partial DONE without CRLF followed by timeout closes with BYE", func(t *testing.T) {
		w := dialFramingIdleServer(t, func(ln net.Listener) net.Listener {
			return authenticateDeadlineListener{ln}
		})
		_ = w.conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprint(w.conn, "a2 IDLE\r\n")
		if cont := w.line(); !strings.HasPrefix(cont, "+ ") {
			t.Fatalf("IDLE continuation = %q, want '+ '", cont)
		}
		fmt.Fprint(w.conn, "DONE")
		time.Sleep(650 * time.Millisecond)
		_, _ = io.WriteString(w.conn, "x1 CREATE injected-mailbox\r\n")
		_ = w.conn.(*net.TCPConn).CloseWrite()
		raw, _ := io.ReadAll(w.rd)
		out := string(raw)
		if strings.Contains(out, "a2 OK ") {
			t.Fatalf("partial 'DONE' without CRLF completed IDLE on read timeout:\n%s", out)
		}
		if strings.Contains(out, "x1 ") {
			t.Fatalf("post-timeout continuation of unfinished IDLE line executed as command:\n%s", out)
		}
	})
}

func TestWireUnauthenticatedUnknownCommandDesyncSendsSingleBye(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
	}{
		{"nonsync literal", "a1 FOO {5+}\r\nhello\r\n"},
		{"bare CR", "a1 FOO bar\rbaz\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialFramingUnauthServer(t, "user1", "pass1")
			_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.WriteString(w.conn, tc.wire); err != nil {
				t.Fatal(err)
			}
			_ = w.conn.(*net.TCPConn).CloseWrite()
			raw, err := io.ReadAll(w.rd)
			if err != nil {
				t.Fatalf("read: %v (output %q)", err, raw)
			}
			out := string(raw)
			if got := strings.Count(out, "* BYE "); got != 1 {
				t.Fatalf("after unauthenticated unknown desynced command server sent %d BYEs, want 1:\n%s", got, out)
			}
		})
	}
}

func TestWireLeadingBareCROrSpaceIsNotValidCommandPrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
	}{
		{"leading bare CR", "\ra2 CREATE injected-mailbox\r\n"},
		{"leading space", " a2 CREATE injected-mailbox\r\n"},
		{"leading space and bare CR", " \ra2 CREATE injected-mailbox\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialFramingAuthServer(t)
			_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.WriteString(w.conn, tc.wire); err != nil {
				t.Fatal(err)
			}
			_ = w.conn.(*net.TCPConn).CloseWrite()
			raw, err := io.ReadAll(w.rd)
			if err != nil {
				t.Fatalf("read: %v (output %q)", err, raw)
			}
			out := string(raw)
			if strings.Contains(out, "a2 OK ") {
				t.Fatalf("command with %s was accepted and executed:\n%s", tc.name, out)
			}
		})
	}
}

func TestWireOversizedCommandInDiscardLineSendsTooLongBye(t *testing.T) {
	c, _, s := newBadAndByeCondStoreConn(t, false, 3, true, imap.CapQResync, imap.CapNotify)
	c.appendMessage("append2")
	c.do("enable", "ENABLE QRESYNC")
	c.do("select", "SELECT INBOX")
	c.do("del", `STORE 2 +FLAGS.SILENT (\Deleted)`)
	c.do("notify", "NOTIFY SET (SELECTED-DELAYED (MessageNew MessageExpunge FlagChange))")

	close(s.trigger)
	<-s.written

	prefix := "x1 NOOP "
	padLen := (50 * 1024) + 3 - len(prefix)
	singleOversizedLine := prefix + strings.Repeat("A", padLen) + "smuggled LOGOUT\r\n"
	if _, err := io.WriteString(c.conn, singleOversizedLine); err != nil {
		t.Fatal(err)
	}

	raw, err := io.ReadAll(c.br)
	if err != nil {
		t.Fatal(err)
	}
	resp := string(raw)
	if strings.Contains(resp, "smuggled ") {
		t.Fatalf("tail of oversized NOOP line executed as command:\n%s", resp)
	}
	if !strings.Contains(resp, "x1 BAD [CLIENTBUG] Command line too long") || !strings.Contains(resp, "* BYE Command too long") {
		t.Fatalf("expected x1 BAD Command line too long + * BYE Command too long, got:\n%s", resp)
	}
	if got := strings.Count(resp, "* BYE "); got != 1 {
		t.Fatalf("expected 1 BYE, got %d:\n%s", got, resp)
	}
}

func dialFramingUnauthServer(t *testing.T, username, password string) *parseWire {
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
		Logger:       log.New(io.Discard, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
		srv.Close()
	})
	go srv.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	w := &parseWire{t: t, conn: c, rd: bufio.NewReader(c)}
	w.line()
	return w
}

func dialFramingAuthServer(t *testing.T) *parseWire {
	t.Helper()
	w := dialFramingUnauthServer(t, "user1", "pass1")
	fmt.Fprint(w.conn, "a1 LOGIN user1 pass1\r\n")
	if got := w.until("a1"); !strings.HasPrefix(got, "a1 OK ") {
		t.Fatalf("LOGIN = %q, want a1 OK", got)
	}
	return w
}

func dialFramingIdleServer(t *testing.T, wrap func(net.Listener) net.Listener) *parseWire {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser("user1", "pass1")
	if err := u.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}, imap.CapLiteralPlus: {}},
		Logger:       log.New(io.Discard, "", 0),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		ln = wrap(ln)
	}
	t.Cleanup(func() {
		ln.Close()
		srv.Close()
	})
	go srv.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	w := &parseWire{t: t, conn: c, rd: bufio.NewReader(c)}
	w.line()
	fmt.Fprint(w.conn, "a1 LOGIN user1 pass1\r\n")
	if got := w.until("a1"); !strings.HasPrefix(got, "a1 OK ") {
		t.Fatalf("LOGIN = %q, want a1 OK", got)
	}
	return w
}
