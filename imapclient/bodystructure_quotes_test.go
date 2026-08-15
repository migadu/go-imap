package imapclient

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadBodyStructureStrayQuotes is a regression test for one malformed field
// failing an entire FETCH response.
//
// 163.com writes an empty body-fld-desc as "" "" -- four quote bytes, which is
// not a valid quoted string. Quoted stopped at the second quote and left the
// other two on the wire, so every following field decoded against the wrong
// offsets and the response died with
//
//	in body-type-mpart: in body-type-1part: imapwire: expected SP, got "\""
//
// The description is cosmetic, but the failure took down the whole response,
// including BODY[] of every message in it.
//
// Upstream report: emersion/go-imap#659, fix proposed in emersion/go-imap#679
// by xiaoquanidea.
func TestReadBodyStructureStrayQuotes(t *testing.T) {
	// Verbatim from the upstream report.
	const data = `(("text" "html" ("charset" "UTF-8") NIL NIL "quoted-printable" 1224 16)("application" "octet-stream" ("name" "2154973550.pdf") NIL """" "base64" 65322) "MIXED")`

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

	// The part carrying the malformed description must still decode, and the
	// fields after it must not be shifted.
	pdf, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("Children[1] = %T, want *imap.BodyStructureSinglePart", mp.Children[1])
	}
	if pdf.Description != "" {
		t.Errorf("Description = %q, want empty", pdf.Description)
	}
	if pdf.Encoding != "base64" {
		t.Errorf("Encoding = %q, want %q", pdf.Encoding, "base64")
	}
	if pdf.Size != 65322 {
		t.Errorf("Size = %v, want 65322", pdf.Size)
	}
	if got := pdf.Params["name"]; got != "2154973550.pdf" {
		t.Errorf("Params[name] = %q, want %q", got, "2154973550.pdf")
	}
}

// TestReadBodyStructureStrayQuotesInParam covers the same malformation in a
// body-fld-param value, and the trailing-')' position that a fixed two-byte
// lookahead would miss.
func TestReadBodyStructureStrayQuotesInParam(t *testing.T) {
	const data = `("application" "octet-stream" ("name" """") NIL NIL "base64" 42)`

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	bs, err := readBody(dec, &Options{})
	if err != nil {
		t.Fatalf("readBody() = %v", err)
	}

	sp, ok := bs.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("readBody() = %T, want *imap.BodyStructureSinglePart", bs)
	}
	if got, ok := sp.Params["name"]; !ok || got != "" {
		t.Errorf("Params[name] = %q (present=%v), want empty and present", got, ok)
	}
	if sp.Encoding != "base64" {
		t.Errorf("Encoding = %q, want %q", sp.Encoding, "base64")
	}
	if sp.Size != 42 {
		t.Errorf("Size = %v, want 42", sp.Size)
	}
}

// TestReadBodyStructureQuotingUnaffected pins conformant input: the recovery
// must never fire on a well-formed response. An escaped quote inside a
// description, and a genuinely empty description, must both survive verbatim.
func TestReadBodyStructureQuotingUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{
			name: "escaped quote in description",
			data: `("text" "plain" NIL NIL "say \"hi\"" "7bit" 10 1)`,
			want: `say "hi"`,
		},
		{
			name: "empty description",
			data: `("text" "plain" NIL NIL "" "7bit" 10 1)`,
			want: "",
		},
		{
			name: "ordinary description",
			data: `("text" "plain" NIL NIL "Compiler diff" "7bit" 10 1)`,
			want: "Compiler diff",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.data)), imapwire.ConnSideClient)
			bs, err := readBody(dec, &Options{})
			if err != nil {
				t.Fatalf("readBody() = %v", err)
			}
			sp := bs.(*imap.BodyStructureSinglePart)
			if sp.Description != tc.want {
				t.Errorf("Description = %q, want %q", sp.Description, tc.want)
			}
			if sp.Encoding != "7bit" || sp.Size != 10 {
				t.Errorf("Encoding = %q, Size = %v; want 7bit, 10 (fields shifted)", sp.Encoding, sp.Size)
			}
		})
	}
}
