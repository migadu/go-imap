package imapclient

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadBodyStructureNilLineCount is a regression test for a text part whose
// line count is NIL.
//
// RFC 9051 body-type-text is "media-text SP body-fields SP body-fld-lines", so
// the line count is mandatory and a server sending NIL is malformed. Servers do
// it anyway: a synthetic body structure, built for a message the server could
// not parse or could not load, is typically declared "text" "plain" while its
// line count is left unset, and the serializer then writes the extension
// section where the count belongs:
//
//	("text" "plain" NIL NIL NIL "7bit" 1234 NIL NIL NIL NIL)
//
// The count was read with ExpectNumber64, so the whole FETCH response failed
// with
//
//	in body-type-1part: imapwire: expected number64, got "N"
//
// and one such message took down every other message in the batch with it --
// enough to make a mailbox unopenable in a webmail client that fetches a page
// of messages at a time.
func TestReadBodyStructureNilLineCount(t *testing.T) {
	const data = `("text" "plain" NIL NIL NIL "7bit" 1234 NIL NIL NIL NIL)`

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	bs, err := readBody(dec, &Options{})
	if err != nil {
		t.Fatalf("readBody() = %v", err)
	}

	sp, ok := bs.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("readBody() = %T, want *imap.BodyStructureSinglePart", bs)
	}
	if sp.Type != "text" || sp.Subtype != "plain" {
		t.Errorf("got %v/%v, want text/plain", sp.Type, sp.Subtype)
	}
	if sp.Size != 1234 {
		t.Errorf("Size = %v, want 1234", sp.Size)
	}
	if sp.Text == nil {
		t.Fatal("Text = nil, want a line count of 0")
	}
	if sp.Text.NumLines != 0 {
		t.Errorf("Text.NumLines = %v, want 0", sp.Text.NumLines)
	}
}

// TestReadBodyStructureNilLineCountInMultipart is what the failure actually
// looks like in the wild: the malformed part sits next to healthy siblings, and
// what matters is that they survive it.
func TestReadBodyStructureNilLineCountInMultipart(t *testing.T) {
	const data = `(("text" "plain" NIL NIL NIL "7bit" 1234 NIL NIL NIL NIL)("APPLICATION" "PDF" NIL NIL NIL "BASE64" 1476806 NIL ("ATTACHMENT" ("FILENAME" "x.pdf")) NIL) "MIXED" ("BOUNDARY" "abc") NIL NIL)`

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	bs, err := readBody(dec, &Options{})
	if err != nil {
		t.Fatalf("readBody() = %v", err)
	}

	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("readBody() = %T, want *imap.BodyStructureMultiPart", bs)
	}
	if len(mp.Children) != 2 {
		t.Fatalf("len(Children) = %v, want 2", len(mp.Children))
	}
	pdf, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("Children[1] = %T, want *imap.BodyStructureSinglePart", mp.Children[1])
	}
	if pdf.Size != 1476806 {
		t.Errorf("Children[1].Size = %v, want 1476806", pdf.Size)
	}
	if pdf.Extended == nil || pdf.Extended.Disposition == nil {
		t.Fatalf("Children[1].Extended.Disposition = nil, want the attachment disposition")
	}
	if got := pdf.Extended.Disposition.Params["filename"]; got != "x.pdf" {
		t.Errorf("Children[1] filename = %q, want %q", got, "x.pdf")
	}
}

// TestReadBodyStructureNilLineCountMessageRFC822 covers the other part type
// that carries a line count.
func TestReadBodyStructureNilLineCountMessageRFC822(t *testing.T) {
	const data = `("MESSAGE" "RFC822" NIL NIL NIL "7BIT" 4096 (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL) ("TEXT" "PLAIN" NIL NIL NIL "7BIT" 512 10) NIL)`

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	bs, err := readBody(dec, &Options{})
	if err != nil {
		t.Fatalf("readBody() = %v", err)
	}

	sp := bs.(*imap.BodyStructureSinglePart)
	if sp.MessageRFC822 == nil {
		t.Fatal("MessageRFC822 = nil, want the embedded message")
	}
	if sp.MessageRFC822.NumLines != 0 {
		t.Errorf("MessageRFC822.NumLines = %v, want 0", sp.MessageRFC822.NumLines)
	}
	child, ok := sp.MessageRFC822.BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("embedded body = %T, want *imap.BodyStructureSinglePart", sp.MessageRFC822.BodyStructure)
	}
	if child.Text == nil || child.Text.NumLines != 10 {
		t.Errorf("embedded Text = %+v, want 10 lines", child.Text)
	}
}

// TestReadBodyStructureLineCountUnaffected pins the conformant cases: a real
// line count is still read as a number, and a token that is neither a number
// nor NIL is still an error rather than being silently taken as zero.
func TestReadBodyStructureLineCountUnaffected(t *testing.T) {
	t.Run("number", func(t *testing.T) {
		const data = `("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 1152 23)`

		dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
		bs, err := readBody(dec, &Options{})
		if err != nil {
			t.Fatalf("readBody() = %v", err)
		}
		sp := bs.(*imap.BodyStructureSinglePart)
		if sp.Text == nil || sp.Text.NumLines != 23 {
			t.Errorf("Text = %+v, want 23 lines", sp.Text)
		}
	})

	t.Run("garbage", func(t *testing.T) {
		const data = `("TEXT" "PLAIN" NIL NIL NIL "7BIT" 1152 LINES)`

		dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
		if _, err := readBody(dec, &Options{}); err == nil {
			t.Fatal("readBody() = nil, want an error for a non-numeric line count")
		} else if !strings.Contains(err.Error(), "body-fld-lines") {
			t.Errorf("readBody() = %v, want a body-fld-lines error", err)
		}
	})
}
