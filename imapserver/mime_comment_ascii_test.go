package imapserver

import (
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

func TestMIMEHeaderTokenASCIICommentsAndNBSP(t *testing.T) {
	t.Run("rfc822_comments_when_parser_strips_comments", func(t *testing.T) {
		for _, tc := range []struct {
			raw    string
			parsed string
			want   string
		}{
			{
				raw:    "(comment) multipart/mixed; boundary=x",
				parsed: "multipart/mixed",
				want:   "multipart/mixed",
			},
			{
				raw:    "text/plain (caf\u00e9); charset=utf-8",
				parsed: "text/plain",
				want:   "text/plain",
			},
			{
				raw:    "text/plain (charset=utf-8; format=flowed); charset=utf-8",
				parsed: "text/plain",
				want:   "text/plain",
			},
			{
				raw:    "text (c1) / (c2) plain; charset=utf-8",
				parsed: "text/plain",
				want:   "text/plain",
			},
			{
				raw:    "inline (caf\u00e9); filename=\"note.txt\"",
				parsed: "inline",
				want:   "inline",
			},
			{
				raw:    "(; ) mult\u0130part/mixed; boundary=x",
				parsed: "multipart/mixed",
				want:   "mult\u0130part/mixed",
			},
		} {
			if got := mimeHeaderTokenASCII(tc.raw, tc.parsed, nil); got != tc.want {
				t.Errorf("mimeHeaderTokenASCII(%q, %q, nil) = %q, want %q", tc.raw, tc.parsed, got, tc.want)
			}
		}
	})

	t.Run("cte_comment_preserves_non_ascii_nbsp", func(t *testing.T) {
		const rawNoComment = "Content-Type: text/plain\r\nContent-Transfer-Encoding: \u00a0base64\r\n\r\nbGluZTE=\r\n"
		if _, err := DecodeBinarySection(strings.NewReader(rawNoComment), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, ErrUnknownCTE) {
			t.Fatalf("without comment, NBSP CTE err = %v, want ErrUnknownCTE", err)
		}

		const rawWithComment = "Content-Type: text/plain\r\nContent-Transfer-Encoding: ()\u00a0base64\r\n\r\nbGluZTE=\r\n"
		if got, err := DecodeBinarySection(strings.NewReader(rawWithComment), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, ErrUnknownCTE) {
			t.Errorf("with empty comment, NBSP CTE DecodeBinarySection = %q, %v; want ErrUnknownCTE", got, err)
		}
	})
}
