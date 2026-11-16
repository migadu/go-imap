package imapserver_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// Some go-message replacements retain a usable, normalized media type when
// only its parameters fail to parse. That error must not allow Unicode case
// folding to classify an unknown token as a known MIME type or disposition.
// The default dependency may instead retain the entire raw field on error;
// both representations must preserve the unknown token's semantics.
func TestMIMEParameterErrorDoesNotFoldNonASCIITokens(t *testing.T) {
	t.Run("primary_type_binary_and_structure", func(t *testing.T) {
		const raw = "Content-Type: multİpart/mixed; boundary=x; bad\r\n" +
			"Content-Transfer-Encoding: QUOTED-PRİNTABLE\r\n\r\n" +
			"--x\r\nContent-Type: text/plain\r\n\r\nA=42\r\n--x--\r\n"
		for _, part := range [][]int{nil, {1}} {
			got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: part})
			if !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("BINARY%v = %q, %v; want UNKNOWN-CTE", part, got, err)
			}
			if size, err := imapserver.DecodeBinarySectionSize(strings.NewReader(raw), &imap.FetchItemBinarySectionSize{Part: part}); !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("BINARY.SIZE%v = %d, %v; want UNKNOWN-CTE", part, size, err)
			}
		}
		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw))
		if _, ok := bs.(*imap.BodyStructureSinglePart); !ok {
			t.Fatalf("unknown Unicode primary type became a multipart: %+v", bs)
		}
		if strings.HasPrefix(bs.MediaType(), "multipart/") {
			t.Errorf("unknown Unicode primary type folded to %q", bs.MediaType())
		}
	})

	t.Run("disposition_metadata", func(t *testing.T) {
		const raw = "Content-Type: text/plain\r\n" +
			"Content-Disposition: İnlİne; filename=\"private.txt\"; bad\r\n\r\nbody\r\n"
		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw)).(*imap.BodyStructureSinglePart)
		disp := bs.Extended.Disposition
		if disp == nil || !strings.HasPrefix(disp.Value, "İnlİne") {
			t.Errorf("unknown Unicode disposition folded to a known token: %+v", disp)
		}
	})
}
