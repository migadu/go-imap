package imapclient

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadBodyStructureChildlessMultipart is a regression test for a multipart
// body carrying no child parts.
//
// RFC 9051 body-type-mpart is "1*body SP media-subtype [SP body-ext-mpart]", so
// at least one child is required and a server emitting none is malformed. Some
// do anyway, for an alternative part that ended up empty:
//
//	("ALTERNATIVE" ("BOUNDARY" "...") NIL NIL)
//
// A body starting with a string was always read as body-type-1part, so the
// media subtype was looked for where the parameter list sits, and the whole
// FETCH response failed with
//
//	in body-type-mpart: in body-type-1part: imapwire: expected string, got "("
//
// One empty part costs the entire response, including BODY[] of every message
// in it, and in the report the sibling part was a 1.4 MB PDF attachment.
//
// Upstream report: emersion/go-imap#701 by Fizzadar.
func TestReadBodyStructureChildlessMultipart(t *testing.T) {
	// Verbatim from the upstream report.
	const data = `(("ALTERNATIVE" ("BOUNDARY" "e7e3f78cb2203c4e50561d13c480670d46c0bb5ccba09400aae9c221fc41") NIL NIL)("APPLICATION" "PDF" NIL NIL NIL "BASE64" 1476806 NIL ("ATTACHMENT" ("FILENAME" "investing-101.pdf")) NIL) "MIXED" ("BOUNDARY" "7b1fa0fc24c33f620e26ce902dcf2ecbcc7ba56cd9e042624b2ee60b9d56") NIL NIL)`

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	bs, err := readBody(dec, &Options{})
	if err != nil {
		t.Fatalf("readBody() = %v", err)
	}

	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("readBody() = %T, want *imap.BodyStructureMultiPart", bs)
	}
	if mp.Subtype != "MIXED" {
		t.Errorf("Subtype = %q, want %q", mp.Subtype, "MIXED")
	}
	if len(mp.Children) != 2 {
		t.Fatalf("len(Children) = %v, want 2", len(mp.Children))
	}

	// The empty alternative comes back as a multipart with no children.
	alt, ok := mp.Children[0].(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("Children[0] = %T, want *imap.BodyStructureMultiPart", mp.Children[0])
	}
	if alt.Subtype != "ALTERNATIVE" {
		t.Errorf("Children[0].Subtype = %q, want %q", alt.Subtype, "ALTERNATIVE")
	}
	if len(alt.Children) != 0 {
		t.Errorf("len(Children[0].Children) = %v, want 0", len(alt.Children))
	}
	if alt.Extended == nil || alt.Extended.Params["boundary"] == "" {
		t.Errorf("Children[0].Extended = %#v, want the boundary parameter", alt.Extended)
	}

	// What actually matters: the sibling part after it is still read correctly.
	pdf, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("Children[1] = %T, want *imap.BodyStructureSinglePart", mp.Children[1])
	}
	if pdf.Subtype != "PDF" {
		t.Errorf("Children[1].Subtype = %q, want %q", pdf.Subtype, "PDF")
	}
	if pdf.Size != 1476806 {
		t.Errorf("Children[1].Size = %v, want 1476806", pdf.Size)
	}
	if pdf.Extended == nil || pdf.Extended.Disposition == nil {
		t.Fatalf("Children[1].Extended.Disposition = nil, want the attachment disposition")
	}
	if got := pdf.Extended.Disposition.Params["filename"]; got != "investing-101.pdf" {
		t.Errorf("Children[1] filename = %q, want %q", got, "investing-101.pdf")
	}
}

// TestReadBodyStructureChildlessMultipartBare covers the same shape with no
// extension data at all, where there is nothing after the subtype.
func TestReadBodyStructureChildlessMultipartBare(t *testing.T) {
	const data = `(("ALTERNATIVE")("TEXT" "PLAIN" NIL NIL NIL "7BIT" 10 1) "MIXED")`

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	bs, err := readBody(dec, &Options{})
	if err != nil {
		t.Fatalf("readBody() = %v", err)
	}

	mp := bs.(*imap.BodyStructureMultiPart)
	if len(mp.Children) != 2 {
		t.Fatalf("len(Children) = %v, want 2", len(mp.Children))
	}
	alt, ok := mp.Children[0].(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("Children[0] = %T, want *imap.BodyStructureMultiPart", mp.Children[0])
	}
	if alt.Subtype != "ALTERNATIVE" || len(alt.Children) != 0 {
		t.Errorf("Children[0] = %+v, want an empty ALTERNATIVE", alt)
	}
	if txt := mp.Children[1].(*imap.BodyStructureSinglePart); txt.Subtype != "PLAIN" {
		t.Errorf("Children[1].Subtype = %q, want %q", txt.Subtype, "PLAIN")
	}
}

// TestReadBodyStructureSinglePartUnaffected pins conformant single parts: the
// media type and subtype must keep being read as such, not mistaken for a
// childless multipart.
func TestReadBodyStructureSinglePartUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name         string
		data         string
		typ, subtype string
		size         uint32
	}{
		{
			name: "text/plain", data: `("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 1152 23)`,
			typ: "TEXT", subtype: "PLAIN", size: 1152,
		},
		{
			name: "application/pdf with disposition", data: `("APPLICATION" "PDF" NIL NIL NIL "BASE64" 1476806 NIL ("ATTACHMENT" ("FILENAME" "x.pdf")) NIL)`,
			typ: "APPLICATION", subtype: "PDF", size: 1476806,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.data)), imapwire.ConnSideClient)
			bs, err := readBody(dec, &Options{})
			if err != nil {
				t.Fatalf("readBody() = %v", err)
			}
			sp, ok := bs.(*imap.BodyStructureSinglePart)
			if !ok {
				t.Fatalf("readBody() = %T, want *imap.BodyStructureSinglePart", bs)
			}
			if sp.Type != tc.typ || sp.Subtype != tc.subtype || sp.Size != tc.size {
				t.Errorf("got %v/%v size %v, want %v/%v size %v", sp.Type, sp.Subtype, sp.Size, tc.typ, tc.subtype, tc.size)
			}
		})
	}
}
