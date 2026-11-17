package imapmemserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	gomessage "github.com/emersion/go-message"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

func TestMIMECommentedCTEDecodesCharsetOnce(t *testing.T) {
	old := gomessage.CharsetReader
	t.Cleanup(func() { gomessage.CharsetReader = old })
	gomessage.CharsetReader = func(charset string, r io.Reader) (io.Reader, error) {
		switch charset {
		case "iso-8859-1":
			return charmap.ISO8859_1.NewDecoder().Reader(r), nil
		case "utf-16le":
			return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().Reader(r), nil
		default:
			return nil, fmt.Errorf("unknown charset %q", charset)
		}
	}
	for _, tc := range []struct {
		cte, charset, body string
		want               string
	}{
		{"8bit", "iso-8859-1", "caf\xe9", "café"},
		{"8bit (comment)", "iso-8859-1", "caf\xe9", "café"},
		{"binary (comment)", "iso-8859-1", "caf\xe9", "café"},
		{"base64 (comment)", "utf-16le", "YwBhAGYA6QA=", "café"},
		{"base64 (café)", "utf-8", "Y2Fmw6k=", "café"},
		{"8BİT (comment)", "iso-8859-1", "caf\xe9", ""},
		{"8bit (unclosed", "iso-8859-1", "caf\xe9", ""},
		{"8bit (comment)", "x-unknown", "caf\xe9", ""},
	} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/nested=%v", tc.cte, tc.charset, nested), func(t *testing.T) {
				raw := "X-Note: original part header\r\n" + previewTestEntity("text/plain; charset="+tc.charset, tc.cte, "", tc.body)
				wantPreview := tc.want
				if nested {
					raw = previewTestMultipart("outer", raw, previewTestEntity("text/plain; charset=utf-8", "7bit", "", "fallback text"))
					if wantPreview == "" {
						wantPreview = "fallback text"
					}
				}
				got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
				if err != nil || !available || got != wantPreview {
					t.Errorf("PREVIEW = (%q,%v,%v), want (%q,true,nil)", got, available, err, wantPreview)
				}

				ctx := context.Background()
				u := NewUser("u", "p")
				if err := u.Create(ctx, "INBOX", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := u.Append(ctx, "INBOX", strings.NewReader(raw), &imap.AppendOptions{}); err != nil {
					t.Fatal(err)
				}
				mbox, err := u.mailbox("INBOX")
				if err != nil {
					t.Fatal(err)
				}
				for _, needle := range []string{"café", "cafÃ©"} {
					res, err := mbox.NewView().Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Body: []string{needle}}, &imap.SearchOptions{})
					if err != nil {
						t.Fatal(err)
					}
					want := 0
					if needle == tc.want {
						want = 1
					}
					if nums := res.AllSeqNums(); len(nums) != want {
						t.Errorf("SEARCH BODY %q = %v, want %d matches", needle, nums, want)
					}
				}
				res, err := mbox.NewView().Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Text: []string{"original part header"}}, &imap.SearchOptions{})
				if err != nil || len(res.AllSeqNums()) != 1 {
					t.Errorf("SEARCH TEXT lost original header: result=%v err=%v", res.AllSeqNums(), err)
				}
				mbox.mutex.Lock()
				unchanged := bytes.Equal(mbox.l[0].buf, []byte(raw))
				mbox.mutex.Unlock()
				if !unchanged {
					t.Error("MIME decoding changed stored message bytes")
				}
			})
		}
	}
}

func TestMIMEDecodingKeepsRootHeaderLimit(t *testing.T) {
	msg := &message{buf: []byte("X-Pad: " + strings.Repeat("x", 1<<20) + "\r\n\r\nbody")}
	if _, err := msg.reader(); err == nil {
		t.Fatal("root MIME header over 1MiB was accepted")
	}
	body := strings.Repeat("x", (1<<20)+1)
	msg = &message{buf: []byte("Content-Type: text/plain\r\n\r\n" + body)}
	e, err := msg.reader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(e.Body)
	if err != nil || string(got) != body {
		t.Fatalf("body was capped by header limit: bytes=%d err=%v", len(got), err)
	}
}
