package imapserver

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
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

// leafSize returns the BODYSTRUCTURE size of section "1" (the message itself for
// a single part, or the first child of a multipart).
func leafSize(bs imap.BodyStructure) uint32 {
	switch v := bs.(type) {
	case *imap.BodyStructureSinglePart:
		return v.Size
	case *imap.BodyStructureMultiPart:
		if len(v.Children) > 0 {
			if sp, ok := v.Children[0].(*imap.BodyStructureSinglePart); ok {
				return sp.Size
			}
		}
	}
	return 0
}

// TestExtractSection_ConsistentWithBodyStructure locks the invariant that a part
// BODYSTRUCTURE advertises can actually be fetched: BODY[1] must return the same
// number of bytes ExtractBodyStructure reports for section 1, and BINARY[1] must
// not be empty for it, even for malformed input. ExtractBodyStructure is lenient
// (partial header, io.ReadAll discards errors); ExtractBodySection and
// ExtractBinarySection must be too, or clients see an advertised part that
// fetches empty. (These fixtures are identity-encoded, so decoded == encoded.)
func TestExtractSection_ConsistentWithBodyStructure(t *testing.T) {
	cases := map[string]string{
		"well-formed multipart": "Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nhello body\r\n--b--\r\n",
		"multipart missing final boundary": "Content-Type: multipart/mixed; boundary=\"nofinal\"\r\n\r\n" +
			"--nofinal\r\nContent-Type: text/plain\r\n\r\npart one, and then the message just ends\r\n",
		"malformed header block (no colon)": "this header line has no colon\r\n\r\nthe body of the message\r\n",
		"unknown transfer-encoding": "Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: x-weird\r\n\r\nundecodable but present\r\n--b--\r\n",
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			want := leafSize(ExtractBodyStructure(strings.NewReader(msg)))
			got := ExtractBodySection(strings.NewReader(msg), &imap.FetchItemBodySection{Part: []int{1}})
			if uint32(len(got)) != want {
				t.Errorf("BODY[1] = %d bytes (%q), but BODYSTRUCTURE size = %d", len(got), string(got), want)
			}
			if want > 0 && len(got) == 0 {
				t.Errorf("BODY[1] is empty for a part BODYSTRUCTURE advertises as %d bytes", want)
			}
			bin := ExtractBinarySection(strings.NewReader(msg), &imap.FetchItemBinarySection{Part: []int{1}})
			if want > 0 && len(bin) == 0 {
				t.Errorf("BINARY[1] is empty for a part BODYSTRUCTURE advertises as %d bytes", want)
			}
		})
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

// MIME fields "can include RFC 822 comments, which have no semantic content
// and should be ignored during MIME processing" (RFC 2045 §1), so body-fld-enc
// is the mechanism alone: a client cannot decode "base64 (encoded)".
func TestExtractBodyStructureCTEComments(t *testing.T) {
	msg := "Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64 (encoded)\r\n\r\naGVsbG8gd29ybGQK\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\nContent-Transfer-Encoding: 7bit (embedded)\r\n\r\n" +
		"Subject: inner\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: (a (nested \\) comment)) Quoted-Printable\r\n\r\ncaf=E9\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: x-weird (unknown)\r\n\r\nencoded\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: (only a comment)\r\n\r\nplain\r\n" +
		"--b--\r\n"

	bs := ExtractBodyStructure(strings.NewReader(msg))
	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok || len(mp.Children) != 4 {
		t.Fatalf("ExtractBodyStructure = %#v, want a multipart with 4 children", bs)
	}
	part := func(bs imap.BodyStructure) *imap.BodyStructureSinglePart {
		sp, ok := bs.(*imap.BodyStructureSinglePart)
		if !ok {
			t.Fatalf("part is %T, want *imap.BodyStructureSinglePart", bs)
		}
		return sp
	}
	rfc822 := part(mp.Children[1])
	if rfc822.MessageRFC822 == nil {
		t.Fatal("message/rfc822 part has no MessageRFC822")
	}
	for _, tc := range []struct {
		name string
		part *imap.BodyStructureSinglePart
		want string
	}{
		{"1", part(mp.Children[0]), "base64"},
		{"2", rfc822, "7bit"},
		{"2.1", part(rfc822.MessageRFC822.BodyStructure), "Quoted-Printable"},
		{"3", part(mp.Children[2]), "x-weird"},
		{"4", part(mp.Children[3]), ""},
	} {
		if tc.part.Encoding != tc.want {
			t.Errorf("part %s: Encoding = %q, want %q", tc.name, tc.part.Encoding, tc.want)
		}
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	writeBodyStructure(imapwire.NewEncoder(bw, imapwire.ConnSideServer), bs, true)
	bw.Flush()
	if out := buf.String(); !strings.Contains(out, `NIL NIL "base64" `) || strings.Contains(out, "(encoded)") {
		t.Errorf("BODYSTRUCTURE = %s, want body-fld-enc \"base64\" without the comment", out)
	}
}

// crlfWriter sees the decoded section in arbitrary chunks, so a CRLF split
// across two writes must stay one line ending.
func TestCRLFWriter(t *testing.T) {
	for _, tc := range []struct {
		chunks []string
		want   string
	}{
		{[]string{"a\nb\n"}, "a\r\nb\r\n"},
		{[]string{"\n\n"}, "\r\n\r\n"},
		{[]string{"a\r\nb\r\n"}, "a\r\nb\r\n"},
		{[]string{"a\r", "\nb"}, "a\r\nb"},
		{[]string{"a", "\n", "\n"}, "a\r\n\r\n"},
		{[]string{"a\r", "b\n"}, "a\rb\r\n"},
		{[]string{"\r\r\n"}, "\r\r\n"},
		{[]string{"", "a"}, "a"},
	} {
		var buf bytes.Buffer
		cw := &crlfWriter{w: &buf}
		for _, chunk := range tc.chunks {
			if n, err := cw.Write([]byte(chunk)); n != len(chunk) || err != nil {
				t.Errorf("%q: Write(%q) = %d, %v", tc.chunks, chunk, n, err)
			}
		}
		if got := buf.String(); got != tc.want {
			t.Errorf("%q: wrote %q, want %q", tc.chunks, got, tc.want)
		}
	}
}

func TestDecodeBinarySection(t *testing.T) {
	const msg = "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nbGluZTEKbGluZTIK\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: x-uuencode\r\n\r\nencoded\n\r\n" +
		"--b\r\nContent-Type: multipart/digest; boundary=d\r\n\r\n" +
		"--d\r\n\r\nSubject: digested\n\nbody\n\r\n" +
		"--d--\r\n" +
		"--b--\r\n"
	for _, tc := range []struct {
		part    []int
		partial *imap.SectionPartial
		want    string
		err     error
	}{
		{part: []int{1}, want: "line1\r\nline2\r\n"},
		{part: []int{1}, partial: &imap.SectionPartial{Offset: 5, Size: 4}, want: "\r\nli"},
		{part: []int{2}, err: ErrUnknownCTE},
		// A digest part without a Content-Type is message/rfc822 (RFC 2046
		// §5.1.5), not text, so its octets are kept.
		{part: []int{3, 1}, want: "Subject: digested\n\nbody\n"},
		{part: []int{4}, want: ""},
	} {
		got, err := DecodeBinarySection(strings.NewReader(msg), &imap.FetchItemBinarySection{Part: tc.part, Partial: tc.partial})
		if err != tc.err || string(got) != tc.want {
			t.Errorf("DecodeBinarySection(%v %v) = %q, %v; want %q, %v", tc.part, tc.partial, got, err, tc.want, tc.err)
		}
		size, err := DecodeBinarySectionSize(strings.NewReader(msg), &imap.FetchItemBinarySectionSize{Part: tc.part})
		full, _ := DecodeBinarySection(strings.NewReader(msg), &imap.FetchItemBinarySection{Part: tc.part})
		if err != tc.err || size != uint32(len(full)) {
			t.Errorf("DecodeBinarySectionSize(%v) = %d, %v; want %d, %v", tc.part, size, err, len(full), tc.err)
		}
	}

	// ExtractBinarySection keeps serving an unknown encoding as stored.
	if got := ExtractBinarySection(strings.NewReader(msg), &imap.FetchItemBinarySection{Part: []int{2}}); string(got) != "encoded\n" {
		t.Errorf("ExtractBinarySection([2]) = %q, want %q", got, "encoded\n")
	}
	if got := ExtractBinarySectionSize(strings.NewReader(msg), &imap.FetchItemBinarySectionSize{Part: []int{2}}); got != 8 {
		t.Errorf("ExtractBinarySectionSize([2]) = %d, want 8", got)
	}

	// BINARY[] is the header and the decoded body of the whole message.
	const single = "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nYQpiCg==\r\n"
	const wantSingle = "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\na\r\nb\r\n"
	got, err := DecodeBinarySection(strings.NewReader(single), &imap.FetchItemBinarySection{})
	if err != nil || string(got) != wantSingle {
		t.Errorf("DecodeBinarySection([]) = %q, %v; want %q", got, err, wantSingle)
	}
	if size, err := DecodeBinarySectionSize(strings.NewReader(single), &imap.FetchItemBinarySectionSize{}); err != nil || size != uint32(len(wantSingle)) {
		t.Errorf("DecodeBinarySectionSize([]) = %d, %v; want %d", size, err, len(wantSingle))
	}
}

// MIME fields "can include RFC 822 comments, which have no semantic content
// and should be ignored during MIME processing" (RFC 2045 §1), so a
// Content-Transfer-Encoding the server knows stays decodable with comments
// around it, and only an unknown mechanism is NO [UNKNOWN-CTE].
func TestDecodeBinarySectionCTEComments(t *testing.T) {
	for _, tc := range []struct {
		cte, body, want string
		err             error
	}{
		{cte: "base64 (encoded)", body: "bGluZTEKbGluZTIK\r\n", want: "line1\r\nline2\r\n"},
		{cte: "(encoded) BASE64", body: "bGluZTEKbGluZTIK\r\n", want: "line1\r\nline2\r\n"},
		{cte: "base64(a (nested \\) comment))", body: "bGluZTEKbGluZTIK\r\n", want: "line1\r\nline2\r\n"},
		{cte: "quoted-printable (qp)", body: "caf=E9\r\n", want: "caf\xe9\r\n"},
		{cte: "7bit (plain text)", body: "plain\r\n", want: "plain\r\n"},
		{cte: "(only a comment)", body: "plain\r\n", want: "plain\r\n"},
		{cte: "x-uuencode (comment)", body: "begin 644 a\r\nend\r\n", err: ErrUnknownCTE},
		{cte: "base 64", body: "bGluZTEKbGluZTIK\r\n", err: ErrUnknownCTE},
	} {
		msg := "Content-Type: text/plain\r\nContent-Transfer-Encoding: " + tc.cte + "\r\n\r\n" + tc.body
		got, err := DecodeBinarySection(strings.NewReader(msg), &imap.FetchItemBinarySection{Part: []int{1}})
		if err != tc.err || string(got) != tc.want {
			t.Errorf("CTE %q: DecodeBinarySection = %q, %v; want %q, %v", tc.cte, got, err, tc.want, tc.err)
		}
		size, err := DecodeBinarySectionSize(strings.NewReader(msg), &imap.FetchItemBinarySectionSize{Part: []int{1}})
		if err != tc.err || size != uint32(len(tc.want)) {
			t.Errorf("CTE %q: DecodeBinarySectionSize = %d, %v; want %d, %v", tc.cte, size, err, len(tc.want), tc.err)
		}
	}
}
