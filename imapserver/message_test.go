package imapserver

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestExtractBodyStructure_EmptyMultipart verifies that a multipart message
// whose boundary never matches any part (so the multipart reader yields zero
// children) does NOT panic.  The outer BodyStructureMultiPart must be
// returned, but with a synthetic text/plain child injected so that
// writeBodyTypeMpart satisfies the RFC 3501 "1*body" invariant.  Preserving
// the outer Content-Type is important: IMAP clients use it to understand the
// message structure and must not be silently told the message is text/plain.
func TestExtractBodyStructure_EmptyMultipart(t *testing.T) {
	msg := "Content-Type: multipart/mixed; boundary=\"nonexistent\"\r\n" +
		"\r\n" +
		"This is not a valid MIME part.\r\n"

	bs := ExtractBodyStructure(strings.NewReader(msg))

	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("expected *imap.BodyStructureMultiPart, got %T", bs)
	}
	if mp.Subtype != "mixed" {
		t.Errorf("expected subtype 'mixed', got %q", mp.Subtype)
	}
	if len(mp.Children) != 1 {
		t.Fatalf("expected exactly 1 synthetic child, got %d", len(mp.Children))
	}
	child, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("expected synthetic child to be *imap.BodyStructureSinglePart, got %T", mp.Children[0])
	}
	if child.Type != "text" || child.Subtype != "plain" {
		t.Errorf("expected text/plain synthetic child, got %s/%s", child.Type, child.Subtype)
	}
	if child.Params["charset"] != "utf-8" {
		t.Errorf("expected charset=utf-8 in child params, got %v", child.Params)
	}
	if child.Text == nil {
		t.Fatal("expected Text to be non-nil on synthetic text/plain child")
	}
	if mp.Extended == nil {
		t.Fatal("expected Extended to be non-nil on BodyStructureMultiPart")
	}
}

func TestExtractBodyStructure_EmptyMultipartWithBoundaryBody(t *testing.T) {
	// A multipart message with a body that uses wrong boundaries, so no
	// parts parse. The synthetic child should capture remaining body content.
	msg := "Content-Type: multipart/mixed; boundary=\"correct\"\r\n" +
		"\r\n" +
		"--wrong\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Hello\r\n" +
		"--wrong--\r\n"

	bs := ExtractBodyStructure(strings.NewReader(msg))

	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("expected *imap.BodyStructureMultiPart, got %T", bs)
	}
	if len(mp.Children) != 1 {
		t.Fatalf("expected exactly 1 synthetic child, got %d", len(mp.Children))
	}
	child, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("expected synthetic child to be *imap.BodyStructureSinglePart, got %T", mp.Children[0])
	}
	if child.Type != "text" || child.Subtype != "plain" {
		t.Errorf("expected text/plain, got %s/%s", child.Type, child.Subtype)
	}
	if child.Text == nil {
		t.Fatal("expected Text to be non-nil")
	}
}

// TestExtractBodyStructure_EmptyMultipart_WriteBodyStructure exercises the
// full FETCH write path for a malformed multipart.  It calls writeBodyStructure
// (the function used by FetchResponseWriter.WriteBodyStructure) in both
// non-extended (BODY) and extended (BODYSTRUCTURE) modes and verifies that
// neither panics and that the serialised output preserves the multipart subtype.
func TestExtractBodyStructure_EmptyMultipart_WriteBodyStructure(t *testing.T) {
	msg := "Content-Type: multipart/mixed; boundary=\"nonexistent\"\r\n" +
		"\r\n" +
		"This is not a valid MIME part.\r\n"

	bs := ExtractBodyStructure(strings.NewReader(msg))

	for _, extended := range []bool{false, true} {
		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		enc := imapwire.NewEncoder(bw, imapwire.ConnSideServer)
		// Must not panic for either BODY (extended=false) or BODYSTRUCTURE (extended=true).
		writeBodyStructure(enc, bs, extended)
		bw.Flush()
		out := buf.String()
		if !strings.Contains(out, "mixed") {
			t.Errorf("extended=%v: expected 'mixed' subtype in output, got: %s", extended, out)
		}
		if !strings.Contains(out, "text") {
			t.Errorf("extended=%v: expected synthetic 'text' child in output, got: %s", extended, out)
		}
	}
}

// TestWriteBodyStructure_StaleEmptyMultipart simulates a backend that
// previously cached a BodyStructureMultiPart with zero children (produced
// before extractBodyStructure was fixed) and now replays it directly into
// writeBodyStructure.  Both BODY and BODYSTRUCTURE modes must not panic.
func TestWriteBodyStructure_StaleEmptyMultipart(t *testing.T) {
	stale := &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		// Children intentionally empty — this is the "old bad cached state".
		Extended: &imap.BodyStructureMultiPartExt{
			Params: map[string]string{"boundary": "old"},
		},
	}

	for _, extended := range []bool{false, true} {
		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		enc := imapwire.NewEncoder(bw, imapwire.ConnSideServer)
		// Must not panic even though Children is empty.
		writeBodyStructure(enc, stale, extended)
		bw.Flush()
		out := buf.String()
		if !strings.Contains(out, "mixed") {
			t.Errorf("extended=%v: expected 'mixed' subtype in output, got: %s", extended, out)
		}
	}
}

func TestExtractBodyStructure_ValidMultipart(t *testing.T) {
	// A well-formed multipart/mixed message should still return a
	// BodyStructureMultiPart with the real parsed children.
	msg := "Content-Type: multipart/mixed; boundary=\"boundary42\"\r\n" +
		"\r\n" +
		"--boundary42\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Hello world\r\n" +
		"--boundary42--\r\n"

	bs := ExtractBodyStructure(strings.NewReader(msg))

	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("expected *imap.BodyStructureMultiPart, got %T", bs)
	}
	if len(mp.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(mp.Children))
	}
	child, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("expected child to be *imap.BodyStructureSinglePart, got %T", mp.Children[0])
	}
	if child.Type != "text" || child.Subtype != "plain" {
		t.Errorf("expected text/plain child, got %s/%s", child.Type, child.Subtype)
	}
}
