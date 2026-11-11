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

// previewConn is a raw connection to an in-memory server, logged in, with
// INBOX selected and one message (storeTestMessage, body "Hi") in it.
type previewConn struct {
	t        *testing.T
	conn     net.Conn
	br       *bufio.Reader
	tags     int
	greeting string
}

func dialPreview(t *testing.T, caps imap.CapSet, wrap func(imapserver.Session) imapserver.Session, beforeSelect ...string) *previewConn {
	t.Helper()
	user := imapmemserver.NewUser("ada", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Append(context.Background(), "INBOX", strings.NewReader(storeTestMessage), &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			var session imapserver.Session = mem.NewSession()
			if wrap != nil {
				session = wrap(session)
			}
			return session, nil, nil
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
	c := &previewConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if c.greeting, err = c.br.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	c.mustOK("LOGIN ada pass")
	for _, command := range beforeSelect {
		c.mustOK(command)
	}
	c.mustOK("SELECT INBOX")
	return c
}

// run sends a command and returns its untagged lines and its tagged reply.
func (c *previewConn) run(command string) (untagged string, tagged string) {
	c.t.Helper()
	c.tags++
	tag := fmt.Sprintf("T%d", c.tags)
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, command); err != nil {
		c.t.Fatalf("%s: %v", command, err)
	}
	var lines []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("%s: %v (read so far %q)", command, err, lines)
		}
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, tag+" "); ok {
			return strings.Join(lines, "\n"), rest
		}
		lines = append(lines, line)
	}
}

func (c *previewConn) mustOK(command string) string {
	c.t.Helper()
	untagged, tagged := c.run(command)
	if !strings.HasPrefix(tagged, "OK") {
		c.t.Fatalf("%s: %s", command, tagged)
	}
	return untagged
}

var previewCaps = imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}, imap.CapPreview: {}}

// TestFetchPreviewSyntax covers the PREVIEW fetch-att and its modifier list
// (RFC 8970 §6) in the list and bare forms, including the bare form followed
// by the command's own fetch-modifiers, whose parenthesis must not be taken
// for PREVIEW's.
func TestFetchPreviewSyntax(t *testing.T) {
	for _, command := range []string{
		"FETCH 1 PREVIEW",
		"FETCH 1 preview (lazy)",
		"FETCH 1 PREVIEW (LAZY)",
		"FETCH 1 (PREVIEW (LAZY) UID)",
		"FETCH 1 (UID PREVIEW)",
		"FETCH 1 (PREVIEW (LAZY LAZY))",
		"FETCH 1 PREVIEW (CHANGEDSINCE 1)",
		"FETCH 1 PREVIEW (LAZY) (CHANGEDSINCE 1)",
		"FETCH 1 (PREVIEW) (CHANGEDSINCE 1)",
		"UID FETCH 1 PREVIEW (LAZY)",
	} {
		t.Run(command, func(t *testing.T) {
			c := dialPreview(t, previewCaps, nil)
			untagged := c.mustOK(command)
			if !strings.Contains(untagged, `PREVIEW "Hi"`) {
				t.Errorf("%s answered %q, want PREVIEW \"Hi\"", command, untagged)
			}
		})
	}

	for _, command := range []string{
		"FETCH 1 PREVIEW ()",
		"FETCH 1 PREVIEW (FOO)",
		"FETCH 1 (PREVIEW (LAZY FOO))",
		"FETCH 1 (PREVIEW (LAZYX))",
		"FETCH 1 (PREVIEW (LAZY ))",
		"FETCH 1 PREVIEW (LAZY",
		"FETCH 1 (PREVIEW(LAZY))",
	} {
		t.Run(command, func(t *testing.T) {
			c := dialPreview(t, previewCaps, nil)
			if untagged, tagged := c.run(command); !strings.HasPrefix(tagged, "BAD") {
				t.Errorf("%s answered %q, want BAD (untagged %q)", command, tagged, untagged)
			}
			c.mustOK("NOOP") // the refusal must not desynchronise the stream
		})
	}
}

// TestFetchPreviewShortCommandLines guards the lookahead for "(LAZY": a
// command line that ends right after PREVIEW, or after part of the modifier,
// must be answered at once, not held until the client sends more.
func TestFetchPreviewShortCommandLines(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"FETCH 1 PREVIEW", "OK"},
		{"FETCH 1 (UID PREVIEW)", "OK"},
		{"FETCH 1 PREVIEW (L", "BAD"},
		{"FETCH 1 PREVIEW (LAZ", "BAD"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			c := dialPreview(t, previewCaps, nil)
			if err := c.conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, tagged := c.run(tc.command); !strings.HasPrefix(tagged, tc.want) {
				t.Errorf("%s answered %q, want %s", tc.command, tagged, tc.want)
			}
		})
	}
}

// TestFetchPreviewCapability checks that PREVIEW is advertised, and the data
// item accepted, only when the server is configured with it.
func TestFetchPreviewCapability(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		c := dialPreview(t, previewCaps, nil)
		if caps := c.mustOK("CAPABILITY"); !strings.Contains(caps, " PREVIEW") {
			t.Errorf("CAPABILITY = %q, want PREVIEW", caps)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		c := dialPreview(t, imap.CapSet{imap.CapIMAP4rev1: {}}, nil)
		if caps := c.mustOK("CAPABILITY"); strings.Contains(caps, "PREVIEW") {
			t.Errorf("CAPABILITY = %q, want no PREVIEW", caps)
		}
		if _, tagged := c.run("FETCH 1 PREVIEW"); !strings.HasPrefix(tagged, "BAD") {
			t.Errorf("FETCH 1 PREVIEW answered %q, want BAD", tagged)
		}
		c.mustOK("NOOP")
	})
}

// fixedPreviewSession answers every FETCH with one message carrying the given
// preview, so a test controls exactly what WritePreview is handed.
type fixedPreviewSession struct {
	imapserver.Session
	text      string
	available bool
}

func (s *fixedPreviewSession) Fetch(ctx context.Context, w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	m := w.CreateMessage(1)
	m.WriteUID(1)
	m.WritePreview(s.text, s.available)
	return m.Close()
}

// TestWritePreview covers what reaches the wire: NIL only for a LAZY request
// (RFC 8970 §3: a server MUST NOT return NIL without LAZY), the item only when
// it was requested, and text cut to 256 characters and cleaned of what the
// grammar does not allow.
func TestWritePreview(t *testing.T) {
	long := strings.Repeat("é", 300)
	cut := strings.Repeat("é", 256)
	utf8Caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUTF8Accept: {}, imap.CapPreview: {}}

	for _, tc := range []struct {
		name      string
		caps      imap.CapSet
		enable    string
		command   string
		text      string
		available bool
		want      string // substring of the untagged FETCH response
		wantNot   string
	}{
		{name: "unavailable without LAZY", command: "FETCH 1 PREVIEW", want: `PREVIEW ""`},
		{name: "unavailable with LAZY", command: "FETCH 1 PREVIEW (LAZY)", want: "PREVIEW NIL"},
		{name: "unavailable, one request without LAZY", command: "FETCH 1 (PREVIEW (LAZY) PREVIEW)", want: `PREVIEW ""`},
		{name: "empty", command: "FETCH 1 PREVIEW (LAZY)", available: true, want: `PREVIEW ""`},
		{name: "text", command: "FETCH 1 PREVIEW", text: "Lunch?", available: true, want: `PREVIEW "Lunch?"`},
		{name: "control characters", command: "FETCH 1 PREVIEW", text: "a\x00b\tc\r\nd", available: true, want: `PREVIEW "ab c d"`},
		{name: "invalid UTF-8", command: "FETCH 1 PREVIEW", text: "ok\xffok", available: true, want: `PREVIEW "okok"`},
		{name: "long text as a literal", command: "FETCH 1 PREVIEW", text: long, available: true, want: fmt.Sprintf("PREVIEW {%d}\n%s)", len(cut), cut)},
		{name: "long text quoted in UTF-8", caps: utf8Caps, enable: "ENABLE UTF8=ACCEPT", command: "FETCH 1 PREVIEW", text: long, available: true, want: `PREVIEW "` + cut + `")`},
		{name: "not requested", command: "FETCH 1 (UID)", text: "Lunch?", available: true, wantNot: "PREVIEW"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := tc.caps
			if caps == nil {
				caps = previewCaps
			}
			var beforeSelect []string
			if tc.enable != "" {
				beforeSelect = append(beforeSelect, tc.enable)
			}
			c := dialPreview(t, caps, func(s imapserver.Session) imapserver.Session {
				return &fixedPreviewSession{Session: s, text: tc.text, available: tc.available}
			}, beforeSelect...)
			untagged := c.mustOK(tc.command)
			if tc.want != "" && !strings.Contains(untagged, tc.want) {
				t.Errorf("%s answered %q, want %q", tc.command, untagged, tc.want)
			}
			if tc.wantNot != "" && strings.Contains(untagged, tc.wantNot) {
				t.Errorf("%s answered %q, want no %q", tc.command, untagged, tc.wantNot)
			}
		})
	}
}
