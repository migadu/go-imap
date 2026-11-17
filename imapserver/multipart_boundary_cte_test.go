package imapserver_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestBinaryMultipartWithoutUsableBoundaryChecksCTE(t *testing.T) {
	for _, contentType := range []string{
		"multipart/mixed",
		`multipart/mixed; boundary=""`,
		"multipart/mixed; boundary=x; badparam",
		"multipart/mixed; boundary=",
	} {
		t.Run(contentType, func(t *testing.T) {
			const body = "--x\r\nContent-Type: text/plain\r\n\r\nA=42\n--x--\r\n"
			raw := "Content-Type: " + contentType + "\r\n" +
				"Content-Transfer-Encoding: X-OPAQUE\r\n\r\n" + body
			// Without a usable boundary, part 1 is the opaque body itself.
			if got := imapserver.ExtractBodySection(strings.NewReader(raw), &imap.FetchItemBodySection{Part: []int{1}}); string(got) != body {
				t.Errorf("BODY[1] = %q, want opaque stored body %q", got, body)
			}
			for _, part := range [][]int{nil, {1}} {
				if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: part}); !errors.Is(err, imapserver.ErrUnknownCTE) {
					t.Errorf("BINARY%v = %q, %v; want UNKNOWN-CTE", part, got, err)
				}
				if got, err := imapserver.DecodeBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: part}); !errors.Is(err, imapserver.ErrUnknownCTE) {
					t.Errorf("BINARY.SIZE%v = %d, %v; want UNKNOWN-CTE", part, got, err)
				}
			}
			nested := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\n" + raw + "--outer--\r\n"
			for _, part := range [][]int{{1}, {1, 1}} {
				if got, err := imapserver.DecodeBinarySection(strings.NewReader(nested), &imap.FetchItemBinarySection{Part: part}); !errors.Is(err, imapserver.ErrUnknownCTE) {
					t.Errorf("nested BINARY%v = %q, %v; want UNKNOWN-CTE", part, got, err)
				}
				if got, err := imapserver.DecodeBinarySectionSize(strings.NewReader(nested), &imap.FetchItemBinarySectionSize{Part: part}); !errors.Is(err, imapserver.ErrUnknownCTE) {
					t.Errorf("nested BINARY.SIZE%v = %d, %v; want UNKNOWN-CTE", part, got, err)
				}
			}
			// The deprecated lenient API still exposes the opaque stored bytes.
			if got := imapserver.ExtractBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{}); !strings.HasSuffix(string(got), body) {
				t.Errorf("lenient BINARY[] changed opaque bytes: %q", got)
			}
		})
	}
}

func TestBinaryMultipartWithBoundaryRetainsGenericSubtype(t *testing.T) {
	for _, subtype := range []string{"mixed", "x-custom", "dİgest"} {
		t.Run(subtype, func(t *testing.T) {
			raw := "Content-Type: multipart/" + subtype + "; boundary=x\r\n" +
				"Content-Transfer-Encoding: X-OPAQUE\r\n\r\n" +
				"--x\r\nContent-Type: text/plain; name=\"café.txt\"\r\n" +
				"Content-Transfer-Encoding: base64 (café)\r\n\r\n" +
				"dmlzaWJsZSBib2R5Cg==\r\n--x--\r\n"
			item := &imap.FetchItemBinarySection{Part: []int{1}}
			got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), item)
			if err != nil || string(got) != "visible body\r\n" {
				t.Fatalf("BINARY[1] = %q, %v; want decoded child", got, err)
			}
			if size, err := imapserver.DecodeBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: []int{1}}); err != nil || size != uint32(len(got)) {
				t.Errorf("BINARY.SIZE[1] = %d, %v; want %d", size, err, len(got))
			}
			if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{}); err != nil || !strings.Contains(string(got), "--x\r\n") {
				t.Errorf("BINARY[] = %q, %v; want stored multipart body", got, err)
			}
			bs, ok := imapserver.ExtractBodyStructure(strings.NewReader(raw)).(*imap.BodyStructureMultiPart)
			if !ok || bs.Subtype != subtype || len(bs.Children) != 1 {
				t.Errorf("multipart subtype or children changed: %+v", bs)
			}
		})
	}
}

func TestBodyStructureMultipartMissingBoundaryKeepsLenientFallback(t *testing.T) {
	for _, contentType := range []string{"multipart/mixed", `multipart/mixed; boundary=""`} {
		raw := "Content-Type: " + contentType + "\r\n\r\nopaque body\r\n"
		bs, ok := imapserver.ExtractBodyStructure(strings.NewReader(raw)).(*imap.BodyStructureMultiPart)
		if !ok || bs.Subtype != "mixed" || len(bs.Children) != 1 {
			t.Errorf("existing lenient multipart fallback changed for %q: %+v", contentType, bs)
		}
	}
}
