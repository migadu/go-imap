package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

// These fixtures put meaningful text beyond the traversal limits, rather
// than relying on a malformed message to make preview generation fail.
func previewDeepMIME(depth int) string {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		if i != 0 {
			fmt.Fprintf(&b, "--depth%d\r\n", i-1)
		}
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=depth%d\r\n\r\n", i)
	}
	fmt.Fprintf(&b, "--depth%d\r\nContent-Type: text/plain\r\n\r\nDeep text\r\n", depth-1)
	for i := depth - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "--depth%d--\r\n", i)
	}
	return b.String()
}

func previewWideMIME(parts int) string {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=wide\r\n\r\n")
	for i := 0; i < parts; i++ {
		b.WriteString("--wide\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment\r\n\r\nx\r\n")
	}
	b.WriteString("--wide\r\nContent-Type: text/plain\r\n\r\nLate text\r\n--wide--\r\n")
	return b.String()
}

const previewNormalMIME = "Content-Type: multipart/mixed; boundary=normal\r\n\r\n" +
	"--normal\r\nContent-Type: text/plain\r\nContent-Disposition: attachment\r\n\r\nSkip this attachment\r\n" +
	"--normal\r\nContent-Type: multipart/alternative; boundary=alternative\r\n\r\n" +
	"--alternative\r\nContent-Type: text/html\r\n\r\n<p>Skip HTML</p>\r\n" +
	"--alternative\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Keep=20this=20inline=20text.\r\n--alternative--\r\n--normal--\r\n"

type previewTraversalFixture struct {
	name    string
	raw     string
	want    string
	limited bool
}

func previewTraversalFixtures() []previewTraversalFixture {
	return []previewTraversalFixture{
		{name: "depth limit", raw: previewDeepMIME(128), limited: true},
		{name: "part limit", raw: previewWideMIME(2048), limited: true},
		{
			name: "skipped attachment byte limit",
			raw: "Content-Type: multipart/mixed; boundary=bytes\r\n\r\n" +
				"--bytes\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment\r\n\r\n" +
				strings.Repeat("x", 320<<10) + "\r\n" +
				"--bytes\r\nContent-Type: text/plain\r\n\r\nLate text\r\n--bytes--\r\n",
			limited: true,
		},
		{
			name: "nested header byte limit",
			raw: "Content-Type: multipart/mixed; boundary=header\r\n\r\n" +
				"--header\r\nContent-Type: text/plain\r\nX-Padding: " + strings.Repeat("h", 320<<10) + "\r\n\r\n" +
				"Late text\r\n--header--\r\n",
			limited: true,
		},
		{name: "normal MIME", raw: previewNormalMIME, want: "Keep this inline text."},
		{
			name: "chosen plain prefix kept on raw budget cutoff",
			raw: "Content-Type: multipart/mixed; boundary=tailbudget\r\n\r\n" +
				"--tailbudget\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment\r\n\r\n" +
				strings.Repeat("x", 220<<10) + "\r\n" +
				"--tailbudget\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
				"Kept prefix. " + strings.Repeat("pad ", 12<<10) + "\r\n--tailbudget--\r\n",
			want: ("Kept prefix. " + strings.Repeat("pad ", 60))[:200],
		},
		{
			name: "skipped non-UTF-8 HTML sibling",
			raw: "Content-Type: multipart/alternative; boundary=charset\r\n\r\n" +
				"--charset\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n<p>Caf\xe9</p>\r\n" +
				"--charset\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nUTF-8 sibling text.\r\n--charset--\r\n",
			want: "UTF-8 sibling text.",
		},
		{name: "no plain text", raw: "Content-Type: text/html\r\n\r\n<p>No plain text</p>\r\n"},
		{
			name: "exact 128 parts without plain text",
			raw: "Content-Type: multipart/mixed; boundary=exact128\r\n\r\n" +
				strings.Repeat("--exact128\r\nContent-Type: text/html\r\n\r\n<p>x</p>\r\n", 127) +
				"--exact128--\r\n",
			want: "",
		},
	}
}

func previewTraversalOK(t *testing.T, c *notifyTestConn, command string) string {
	t.Helper()
	response := c.cmd("%s", command)
	lines := strings.Split(strings.TrimRight(response, "\r\n"), "\r\n")
	if tagged := lines[len(lines)-1]; !strings.Contains(tagged, " OK ") {
		t.Fatalf("%s: %q", command, response)
	}
	return response
}

func assertTraversalPreview(t *testing.T, response string, lazy bool, tc previewTraversalFixture) {
	t.Helper()
	if !strings.Contains(response, "UID 1") {
		t.Errorf("FETCH is missing UID 1: %q", response)
	}
	if tc.limited && lazy {
		if !strings.Contains(response, "PREVIEW NIL") {
			t.Errorf("resource-limited LAZY preview = %q, want PREVIEW NIL", response)
		}
		return
	}
	if expected := fmt.Sprintf("PREVIEW %q", tc.want); !strings.Contains(response, expected) {
		t.Errorf("preview = %q, want %s", response, expected)
	}
	if strings.Contains(response, "PREVIEW NIL") {
		t.Errorf("non-LAZY or available preview must not be NIL: %q", response)
	}
}

// A resource refusal must preserve the nstring distinction on the wire:
// LAZY may return NIL, whereas an ordinary request must return a string.
// It must also finish the FETCH response and release the connection writer.
func TestFetchPreviewTraversalLimits(t *testing.T) {
	for _, tc := range previewTraversalFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			addr, user, closer := newNotifyTestServer(t)
			defer closer()
			if err := user.Create(context.Background(), "INBOX", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := user.Append(context.Background(), "INBOX", strings.NewReader(tc.raw), &imap.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
			c := dialNotifyTest(t, addr)
			defer c.close()
			c.login(t)
			previewTraversalOK(t, c, "SELECT INBOX")
			for _, lazy := range []bool{true, false} {
				item := "PREVIEW"
				if lazy {
					item += " (LAZY)"
				}
				response := previewTraversalOK(t, c, "FETCH 1 (UID "+item+")")
				assertTraversalPreview(t, response, lazy, tc)
				previewTraversalOK(t, c, "NOOP")
			}
		})
	}
}

// NOTIFY MessageNew uses the same preview availability decision as FETCH,
// including LAZY refusals, and must not leave the asynchronous writer held.
func TestNotifyPreviewTraversalLimits(t *testing.T) {
	for _, tc := range previewTraversalFixtures() {
		for _, lazy := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/lazy=%v", tc.name, lazy), func(t *testing.T) {
				addr, user, closer := newNotifyTestServer(t)
				defer closer()
				if err := user.Create(context.Background(), "INBOX", nil); err != nil {
					t.Fatal(err)
				}
				c := dialNotifyTest(t, addr)
				defer c.close()
				c.login(t)
				previewTraversalOK(t, c, "SELECT INBOX")
				item := "PREVIEW"
				if lazy {
					item += " (LAZY)"
				}
				previewTraversalOK(t, c, "NOTIFY SET (SELECTED (MessageNew (UID "+item+") MessageExpunge))")
				if _, err := user.Append(context.Background(), "INBOX", strings.NewReader(tc.raw), &imap.AppendOptions{}); err != nil {
					t.Fatal(err)
				}
				exists, fetch := false, ""
				for i := 0; i < 8 && fetch == ""; i++ {
					line, err := c.readLine(5 * time.Second)
					if err != nil {
						t.Fatalf("waiting for preview notification: %v", err)
					}
					if strings.Contains(line, " EXISTS") {
						exists = true
					}
					if strings.Contains(line, " FETCH ") {
						if !exists {
							t.Error("FETCH notification arrived before EXISTS")
						}
						fetch = line
					}
				}
				if fetch == "" {
					t.Fatal("no FETCH preview notification arrived")
				}
				assertTraversalPreview(t, fetch, lazy, tc)
				previewTraversalOK(t, c, "NOOP")
			})
		}
	}
}
