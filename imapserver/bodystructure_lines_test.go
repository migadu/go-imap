package imapserver

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestWriteBodyStructure_TextWithoutLineCount covers a backend handing us a
// "text" part with no line count -- a synthetic structure built for a message
// that could not be parsed or could not be loaded, where only Type and Subtype
// were filled in.
//
// body-fld-lines is mandatory for a text part (RFC 9051 body-type-text). Writing
// nothing put the extension section where the count belongs:
//
//	("text" "plain" NIL NIL NIL "7bit" 1234 NIL NIL NIL NIL)
//
// which clients reject, failing the whole FETCH response and every other message
// in it. Zero is written instead.
func TestWriteBodyStructure_TextWithoutLineCount(t *testing.T) {
	synthetic := &imap.BodyStructureSinglePart{
		Type:    "text",
		Subtype: "plain",
		Size:    1234,
		// Text intentionally nil — this is what the fallback structures look like.
		Extended: &imap.BodyStructureSinglePartExt{},
	}

	for _, tc := range []struct {
		extended bool
		want     string
	}{
		{false, `("text" "plain" NIL NIL NIL "7bit" 1234 0)`},
		{true, `("text" "plain" NIL NIL NIL "7bit" 1234 0 NIL NIL NIL NIL)`},
	} {
		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		enc := imapwire.NewEncoder(bw, imapwire.ConnSideServer)
		writeBodyStructure(enc, synthetic, tc.extended)
		bw.Flush()
		if got := buf.String(); got != tc.want {
			t.Errorf("extended=%v:\n got %v\nwant %v", tc.extended, got, tc.want)
		}
	}
}

// TestWriteBodyStructure_LineCountUnaffected pins the normal case: a part that
// carries a line count still reports it, and a non-text part still has none.
func TestWriteBodyStructure_LineCountUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name string
		bs   *imap.BodyStructureSinglePart
		want string
	}{
		{
			name: "text with line count",
			bs: &imap.BodyStructureSinglePart{
				Type: "text", Subtype: "plain", Encoding: "7bit", Size: 1234,
				Text: &imap.BodyStructureText{NumLines: 23},
			},
			want: `("text" "plain" NIL NIL NIL "7bit" 1234 23)`,
		},
		{
			name: "non-text",
			bs: &imap.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Encoding: "base64", Size: 1234,
			},
			want: `("application" "pdf" NIL NIL NIL "base64" 1234)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			bw := bufio.NewWriter(&buf)
			enc := imapwire.NewEncoder(bw, imapwire.ConnSideServer)
			writeBodyStructure(enc, tc.bs, false)
			bw.Flush()
			if got := buf.String(); got != tc.want {
				t.Errorf("\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}
