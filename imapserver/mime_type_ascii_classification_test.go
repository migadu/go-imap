package imapserver_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// MIME type tokens are ASCII case-insensitive. A non-ASCII lookalike must not
// become multipart before BINARY's strict transfer-encoding check, nor cause
// part [1] to descend past the unknown encoding into a synthetic MIME child.
func TestBinaryNonASCIIMultipartTypeDoesNotBypassUnknownCTE(t *testing.T) {
	for _, mediaType := range []string{"multİpart/mixed", "MULTİPART/MIXED"} {
		raw := "Content-Type: " + mediaType + "; boundary=x\r\n" +
			"Content-Transfer-Encoding: QUOTED-PRİNTABLE\r\n\r\n" +
			"--x\r\nContent-Type: text/plain\r\n\r\nA=42\r\n--x--\r\n"
		for _, part := range [][]int{nil, {1}} {
			item := &imap.FetchItemBinarySection{Part: part}
			got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), item)
			if !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("Content-Type %q BINARY%v = %q, %v; want UNKNOWN-CTE", mediaType, part, got, err)
			}
			if _, err := imapserver.DecodeBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: part}); !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("Content-Type %q BINARY.SIZE%v error = %v, want UNKNOWN-CTE", mediaType, part, err)
			}
		}
		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw))
		if _, ok := bs.(*imap.BodyStructureMultiPart); ok {
			t.Errorf("Content-Type %q became a multipart BODYSTRUCTURE: %+v", mediaType, bs)
		}
	}
}

func TestASCIIMIMETypeRetainsUnicodeParametersAndCTEComments(t *testing.T) {
	const child = "Content-Type: TEXT/PLAIN; charset=utf-8; name=\"café.txt\"\r\n" +
		"Content-Disposition: INLINE; filename=\"café;notes.txt\"\r\n" +
		"Content-Transfer-Encoding: (café (深)) BASE64 (escaped \\) comment)\r\n\r\n" +
		"dmlzaWJsZSBib2R5Cg==\r\n"
	const multipart = "Content-Type: MULTIPART/MIXED; boundary=x\r\nContent-Transfer-Encoding: 7bit\r\n\r\n" +
		"--x\r\n" + child + "--x--\r\n"
	for _, raw := range []string{child, multipart} {
		got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}})
		if err != nil || string(got) != "visible body\r\n" {
			t.Errorf("valid ASCII type with Unicode parameters/comments: BINARY[1] = %q, %v", got, err)
		}
		n, err := imapserver.DecodeBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: []int{1}})
		if err != nil || n != uint32(len("visible body\r\n")) {
			t.Errorf("valid ASCII type: BINARY.SIZE[1] = %d, %v", n, err)
		}
		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw))
		if mp, ok := bs.(*imap.BodyStructureMultiPart); ok {
			if mp.MediaType() != "multipart/mixed" || len(mp.Children) != 1 {
				t.Fatalf("ASCII multipart classification changed: %+v", mp)
			}
			bs = mp.Children[0]
		}
		sp, ok := bs.(*imap.BodyStructureSinglePart)
		if !ok || sp.MediaType() != "text/plain" || sp.Params["name"] != "café.txt" || sp.Filename() != "café;notes.txt" {
			t.Errorf("valid MIME parameters changed: %+v", bs)
		}
	}
	if got, err := imapserver.DecodeBinarySection(strings.NewReader(multipart), &imap.FetchItemBinarySection{}); err != nil || !strings.Contains(string(got), "--x\r\n") {
		t.Errorf("valid ASCII multipart BINARY[] = %q, %v", got, err)
	}
}
