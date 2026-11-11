package imapmemserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func previewTestEntity(contentType, encoding, disposition, body string) string {
	var h strings.Builder
	if contentType != "" {
		fmt.Fprintf(&h, "Content-Type: %s\r\n", contentType)
	}
	if encoding != "" {
		fmt.Fprintf(&h, "Content-Transfer-Encoding: %s\r\n", encoding)
	}
	if disposition != "" {
		fmt.Fprintf(&h, "Content-Disposition: %s\r\n", disposition)
	}
	h.WriteString("\r\n")
	h.WriteString(body)
	return h.String()
}

func previewTestMultipart(boundary string, parts ...string) string {
	var body strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&body, "--%s\r\n%s\r\n", boundary, part)
	}
	fmt.Fprintf(&body, "--%s--\r\n", boundary)
	return previewTestEntity("multipart/mixed; boundary="+boundary, "", "", body.String())
}

func previewTestNested(depth int) string {
	raw := previewTestEntity("text/plain; charset=utf-8", "", "", "leaf")
	for i := 1; i < depth; i++ {
		raw = previewTestMultipart(fmt.Sprintf("level%d", i), raw)
	}
	return raw
}

type previewCountingReader struct {
	r            io.Reader
	bytes, calls int
	afterRead    func()
}

func (r *previewCountingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytes += n
	r.calls++
	if r.afterRead != nil {
		r.afterRead()
	}
	return n, err
}

func TestGeneratePreviewNormalMIME(t *testing.T) {
	plain := previewTestEntity("text/plain; charset=utf-8", "", "", "plain body")
	attachment := previewTestEntity("text/plain", "", "attachment; filename=note.txt", "attachment body")
	html := previewTestEntity("text/html; charset=utf-8", "", "", "<p>HTML body</p>")
	for _, tc := range []struct{ name, raw, want string }{
		{name: "default plain", raw: "Subject: sample\r\n\r\n hello\tworld\r\n", want: "hello world"},
		{name: "Unicode whitespace", raw: previewTestEntity("text/plain; charset=utf-8", "", "", "café\t €\r\nnext\u2003word"), want: "café € next word"},
		{name: "base64", raw: previewTestEntity("text/plain; charset=utf-8", "base64", "", base64.StdEncoding.EncodeToString([]byte("café  €"))), want: "café €"},
		{name: "quoted printable", raw: previewTestEntity("text/plain; charset=utf-8", "quoted-printable", "", "caf=C3=A9=\r\n =E2=82=AC"), want: "café €"},
		{name: "attachment skipped", raw: previewTestMultipart("mixed", attachment, plain), want: "plain body"},
		{name: "nested plain", raw: previewTestMultipart("outer", previewTestMultipart("inner", html, plain)), want: "plain body"},
		{name: "HTML unsupported", raw: html, want: ""},
		{name: "image unsupported", raw: previewTestEntity("image/png", "base64", "", base64.StdEncoding.EncodeToString([]byte("image"))), want: ""},
		{name: "unknown encoding unsupported", raw: previewTestEntity("text/plain", "x-unknown-preview-encoding", "", "undecoded"), want: ""},
		{name: "unknown charset plain unsupported", raw: previewTestEntity("text/plain; charset=iso-8859-1", "", "", "Caf\xe9 "+strings.Repeat("a", 250)), want: ""},
		{name: "skipped sibling unknown charset", raw: previewTestMultipart("alt", previewTestEntity("text/html; charset=iso-8859-1", "", "", "<p>Caf\xe9</p>"), plain), want: "plain body"},
		{name: "skipped sibling unknown encoding", raw: previewTestMultipart("enc", previewTestEntity("application/octet-stream", "x-uuencode", "attachment", "raw"), plain), want: "plain body"},
		{name: "unknown charset plain falls through to UTF-8 plain", raw: previewTestMultipart("fallback", previewTestEntity("text/plain; charset=iso-8859-1", "", "", "Caf\xe9"), plain), want: "plain body"},
		{name: "attachment with unquoted filename space skipped", raw: previewTestMultipart("disp", previewTestEntity("text/plain", "", "attachment; filename=note 2026.txt", "attachment body"), plain), want: "plain body"},
		{name: "code point limit", raw: previewTestEntity("text/plain; charset=utf-8", "", "", strings.Repeat("é", 220)), want: strings.Repeat("é", 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, available, err := generatePreview(context.Background(), strings.NewReader(tc.raw))
			if err != nil || !available || text != tc.want {
				t.Fatalf("preview=(%q,%v,%v), want(%q,true,nil)", text, available, err, tc.want)
			}
		})
	}
}

func TestGeneratePreviewTraversalBoundaries(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		for _, depth := range []int{maxPreviewMIMEDepth, maxPreviewMIMEDepth + 1} {
			t.Run(fmt.Sprint(depth), func(t *testing.T) {
				raw := previewTestNested(depth)
				if len(raw) >= maxPreviewRawBytes {
					t.Fatal("depth fixture also exceeds raw-byte budget")
				}
				text, available, err := generatePreview(context.Background(), strings.NewReader(raw))
				if depth == maxPreviewMIMEDepth {
					if err != nil || !available || text != "leaf" {
						t.Fatalf("boundary leaf=(%q,%v,%v)", text, available, err)
					}
				} else if err != nil || available || text != "" {
					t.Fatalf("over-depth leaf=(%q,%v,%v), want unavailable", text, available, err)
				}
			})
		}
	})
	t.Run("parts including root", func(t *testing.T) {
		for _, total := range []int{maxPreviewMIMEParts, maxPreviewMIMEParts + 1} {
			t.Run(fmt.Sprint(total), func(t *testing.T) {
				parts := make([]string, 0, total-1)
				for i := 0; i < total-2; i++ {
					parts = append(parts, previewTestEntity("application/octet-stream", "", "attachment", "x"))
				}
				parts = append(parts, previewTestEntity("text/plain", "", "", "leaf"))
				raw := previewTestMultipart("parts", parts...)
				if len(raw) >= maxPreviewRawBytes {
					t.Fatal("part-count fixture also exceeds raw-byte budget")
				}
				text, available, err := generatePreview(context.Background(), strings.NewReader(raw))
				if total == maxPreviewMIMEParts {
					if err != nil || !available || text != "leaf" {
						t.Fatalf("boundary leaf=(%q,%v,%v)", text, available, err)
					}
				} else if err != nil || available || text != "" {
					t.Fatalf("over-part leaf=(%q,%v,%v), want unavailable", text, available, err)
				}
			})
		}
	})
}

// Every MIME read shares one raw budget, including library-internal header
// parsing and discarding a previous part before locating the next one.
func TestGeneratePreviewRawBudgetCoversTraversal(t *testing.T) {
	leaf := previewTestEntity("text/plain", "", "", "leaf")
	oversized := strings.Repeat("x", maxPreviewRawBytes+1)
	hugeHeader := "X-Pad: " + oversized + "\r\nContent-Type: text/plain\r\n\r\nleaf"
	preamble := strings.Repeat("preamble\r\n", maxPreviewRawBytes/10+2)
	preambleBody := preamble + "--preamble\r\n" + leaf + "\r\n--preamble--\r\n"
	for _, tc := range []struct{ name, raw string }{
		{name: "top-level header", raw: hugeHeader},
		{name: "nested header", raw: previewTestMultipart("header", hugeHeader)},
		{name: "skipped attachment", raw: previewTestMultipart("attachment", previewTestEntity("application/octet-stream", "", "attachment", oversized), leaf)},
		{name: "skipped HTML", raw: previewTestMultipart("html", previewTestEntity("text/html", "", "", oversized), leaf)},
		{name: "preamble", raw: previewTestEntity("multipart/mixed; boundary=preamble", "", "", preambleBody)},
		{name: "base64 whitespace", raw: previewTestEntity("text/plain", "base64", "", strings.Repeat("\r\n", maxPreviewRawBytes/2+1)+base64.StdEncoding.EncodeToString([]byte("leaf")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &previewCountingReader{r: strings.NewReader(tc.raw)}
			text, available, err := generatePreview(context.Background(), counter)
			if err != nil || available || text != "" {
				t.Errorf("preview=(%q,%v,%v), want resource cutoff", text, available, err)
			}
			if counter.bytes > maxPreviewRawBytes {
				t.Errorf("read %d raw bytes, budget %d", counter.bytes, maxPreviewRawBytes)
			}
		})
	}
}

// The byte limit measures consumption rather than message size: a preview
// already found from a bounded text prefix does not require draining its body.
func TestGeneratePreviewLargePlainPrefix(t *testing.T) {
	header := "Content-Type: text/plain; charset=utf-8\r\n\r\n"
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			body := strings.Repeat("é", (maxPreviewRawBytes-len(header))/2)
			body += strings.Repeat("x", maxPreviewRawBytes-len(header)-len(body)+extra)
			raw := header + body
			if len(raw) != maxPreviewRawBytes+extra {
				t.Fatal("plain-prefix fixture has the wrong raw size")
			}
			counter := &previewCountingReader{r: strings.NewReader(raw)}
			text, available, err := generatePreview(context.Background(), counter)
			if err != nil || !available || text != strings.Repeat("é", 200) {
				t.Fatalf("prefix=(%q,%v,%v)", text, available, err)
			}
			if counter.bytes >= len(raw) || counter.bytes > maxPreviewRawBytes {
				t.Errorf("prefix read %d of %d bytes", counter.bytes, len(raw))
			}
		})
	}
}

func TestPreviewInputExactRawBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			source := strings.NewReader(strings.Repeat("x", maxPreviewRawBytes+extra))
			counter := &previewCountingReader{r: source}
			input := &previewInput{ctx: context.Background(), r: counter, remaining: maxPreviewRawBytes}
			n, err := input.Read(make([]byte, maxPreviewRawBytes+1))
			if n != maxPreviewRawBytes || err != nil {
				t.Fatalf("boundary read=(%d,%v)", n, err)
			}
			calls := counter.calls
			n, err = input.Read(make([]byte, 1))
			if n != 0 || !errors.Is(err, errPreviewBudget) {
				t.Errorf("past-boundary read=(%d,%v), want budget error", n, err)
			}
			if counter.calls != calls || counter.bytes != maxPreviewRawBytes || source.Len() != extra {
				t.Errorf("budget reader probed past cap: calls=%d bytes=%d remaining=%d", counter.calls, counter.bytes, source.Len())
			}
			n, err = input.Read(make([]byte, 1))
			if n != 0 || !errors.Is(err, errPreviewBudget) || counter.calls != calls {
				t.Errorf("budget failure was not sticky: n=%d err=%v calls=%d", n, err, counter.calls)
			}
		})
	}
}

func TestGeneratePreviewCancellation(t *testing.T) {
	t.Run("before any input", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		counter := &previewCountingReader{r: strings.NewReader(previewTestEntity("text/plain", "", "", "body"))}
		text, available, err := generatePreview(ctx, counter)
		if text != "" || available || !errors.Is(err, context.Canceled) || counter.calls != 0 {
			t.Fatalf("cancelled preview=(%q,%v,%v), source calls=%d", text, available, err, counter.calls)
		}
	})
	for _, tc := range []struct {
		name, raw   string
		cancelAfter int
	}{
		{name: "buffered header and text", raw: previewTestEntity("text/plain", "", "", "body already in header buffer"), cancelAfter: 1},
		{name: "implicit attachment discard", raw: previewTestMultipart("cancel", previewTestEntity("application/octet-stream", "", "attachment", strings.Repeat("x", 32<<10)), previewTestEntity("text/plain", "", "", "leaf")), cancelAfter: 2},
		{name: "chosen plain body", raw: previewTestEntity("text/plain", "", "", strings.Repeat("body ", 32<<10)), cancelAfter: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			counter := &previewCountingReader{r: strings.NewReader(tc.raw)}
			counter.afterRead = func() {
				if counter.calls == tc.cancelAfter {
					cancel()
				}
			}
			text, available, err := generatePreview(ctx, counter)
			if text != "" || available || !errors.Is(err, context.Canceled) {
				t.Fatalf("mid-read cancelled preview=(%q,%v,%v)", text, available, err)
			}
			if counter.calls != tc.cancelAfter {
				t.Errorf("cancelled generation made %d source reads, want %d", counter.calls, tc.cancelAfter)
			}
		})
	}
}

// The decoded-text cap must hold independently of the larger raw-input cap.
// Text just beyond the decoded prefix cannot appear in the preview, including
// when a transfer decoder turns a larger raw stream into that prefix.
func TestGeneratePreviewDecodedTextBoundary(t *testing.T) {
	for _, encoding := range []string{"", "base64"} {
		t.Run(encoding, func(t *testing.T) {
			for _, atBoundary := range []bool{false, true} {
				t.Run(fmt.Sprint(atBoundary), func(t *testing.T) {
					spaces := maxPreviewTextBytes - 1
					body, want := strings.Repeat(" ", spaces)+"xy", "x"
					if atBoundary {
						body, want = strings.Repeat(" ", maxPreviewTextBytes)+"y", ""
					}
					if encoding == "base64" {
						body = base64.StdEncoding.EncodeToString([]byte(body))
					}
					raw := previewTestEntity("text/plain", encoding, "", body)
					if len(raw) >= maxPreviewRawBytes {
						t.Fatal("decoded-text fixture also exceeds raw budget")
					}
					text, available, err := generatePreview(context.Background(), strings.NewReader(raw))
					if err != nil || !available || text != want {
						t.Fatalf("decoded prefix=(%q,%v,%v), want(%q,true,nil)", text, available, err, want)
					}
				})
			}
		})
	}
	t.Run("split multibyte rune at text limit", func(t *testing.T) {
		prefix := strings.Repeat("a", 150) + " "
		pad := maxPreviewTextBytes - len(prefix) - 2 // cuts 3-byte '€' after 2 bytes
		body := prefix + strings.Repeat(" ", pad) + "€" + strings.Repeat("z", 100)
		raw := previewTestEntity("text/plain; charset=utf-8", "", "", body)
		text, available, err := generatePreview(context.Background(), strings.NewReader(raw))
		want := strings.Repeat("a", 150)
		if err != nil || !available || text != want {
			t.Fatalf("split rune preview=(%q,%v,%v), want(%q,true,nil)", text, available, err, want)
		}
	})
}
