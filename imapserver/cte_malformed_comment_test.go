package imapserver_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// A lenient MIME decoder may recover a recognized encoding by discarding an
// unfinished comment. Strict BINARY must reject that malformed mechanism, and
// the legacy lenient extraction API must retain its stored bytes instead.
func TestBinaryMalformedCTECommentsRemainUnknown(t *testing.T) {
	const body = "bGluZTEKbGluZTIK\n"
	for _, cte := range []string{
		"base64 (closed) (unterminated",
		"(closed) base64 (unterminated",
		"base64 (outer (closed)",
		"base64 (closed) (escaped\\)",
		"base64 )",
		"base64 (closed))",
	} {
		t.Run(cte, func(t *testing.T) {
			raw := "Content-Type: text/plain\r\nContent-Transfer-Encoding: " + cte + "\r\n\r\n" + body
			item := &imap.FetchItemBinarySection{Part: []int{1}}
			if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), item); !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("strict BINARY[1] = %q, %v; want UNKNOWN-CTE", got, err)
			}
			if n, err := imapserver.DecodeBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: []int{1}}); !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("strict BINARY.SIZE[1] = %d, %v; want UNKNOWN-CTE", n, err)
			}
			if got := imapserver.ExtractBinarySection(strings.NewReader(raw), item); string(got) != body {
				t.Errorf("lenient BINARY[1] changed stored bytes: %q, want %q", got, body)
			}
			if n := imapserver.ExtractBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: []int{1}}); n != uint32(len(body)) {
				t.Errorf("lenient BINARY.SIZE[1] = %d, want stored size %d", n, len(body))
			}
		})
	}
}

func TestBinaryBalancedCTECommentParenthesesStillDecode(t *testing.T) {
	const raw = "Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: BASE64 (café (nested) escaped \\))\r\n\r\n" +
		"bGluZTEKbGluZTIK\r\n"
	got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}})
	if err != nil || string(got) != "line1\r\nline2\r\n" {
		t.Errorf("balanced Unicode/nested/escaped comment = %q, %v", got, err)
	}
}
