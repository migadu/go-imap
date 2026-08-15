package imapclient

import (
	"bufio"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// message/global (RFC 6532) is a single-part body type. RFC 9051 lists it
// alongside message/rfc822 in body-type-msg, so an IMAP4rev2 server follows the
// basic fields with an envelope, a nested body structure and a line count. An
// IMAP4rev1 server has no such rule -- RFC 3501 body-type-msg names only
// message/rfc822 -- so it emits message/global as a plain body-type-basic and
// goes straight to the extension data. Dovecot does the latter.
//
// The parser must therefore decide by what actually follows, not by the
// subtype: an envelope is a parenthesized list, so a '(' means body-type-msg
// and anything else (NIL, a string, end of list) means body-type-basic.
//
// Upstream report: emersion/go-imap#741, fix proposed in emersion/go-imap#704
// by andybalholm. The Dovecot capture below is from that PR.
var bodyStructureCases = []struct {
	name   string
	data   string
	parsed imap.BodyStructure
}{
	{
		// The regression: an IMAP4rev1 server omits envelope/body/lines and
		// goes straight to body-ext-1part. Parsing used to fail here with
		// "in envelope: expected '(', got \"N\"" because the SP before the
		// md5 field was taken as the start of an envelope.
		name: "message/global without body-type-msg fields (IMAP4rev1)",
		data: `("message" "global" ("name" "Untitled attachment 00019.dat") NIL NIL "7bit" 6726 NIL ("attachment" ("filename" "Untitled attachment 00019.dat")) NIL NIL)`,
		parsed: &imap.BodyStructureSinglePart{
			Type:     "message",
			Subtype:  "global",
			Params:   map[string]string{"name": "Untitled attachment 00019.dat"},
			Encoding: "7bit",
			Size:     6726,
			Extended: &imap.BodyStructureSinglePartExt{
				Disposition: &imap.BodyStructureDisposition{
					Value:  "attachment",
					Params: map[string]string{"filename": "Untitled attachment 00019.dat"},
				},
			},
		},
	},
	{
		// An IMAP4rev2 server does send the body-type-msg fields for
		// message/global. This must keep working after the fix.
		name: "message/global with body-type-msg fields (IMAP4rev2)",
		data: `("message" "global" ("name" "fwd.eml") NIL NIL "7bit" 6726 (NIL "Fwd" NIL NIL NIL NIL NIL NIL NIL NIL) ("text" "plain" NIL NIL NIL "7bit" 100 4) 123 NIL NIL NIL NIL)`,
		parsed: &imap.BodyStructureSinglePart{
			Type:     "message",
			Subtype:  "global",
			Params:   map[string]string{"name": "fwd.eml"},
			Encoding: "7bit",
			Size:     6726,
			MessageRFC822: &imap.BodyStructureMessageRFC822{
				Envelope: &imap.Envelope{Subject: "Fwd"},
				BodyStructure: &imap.BodyStructureSinglePart{
					Type:     "text",
					Subtype:  "plain",
					Encoding: "7bit",
					Size:     100,
					Text:     &imap.BodyStructureText{NumLines: 4},
				},
				NumLines: 123,
			},
			Extended: &imap.BodyStructureSinglePartExt{},
		},
	},
	{
		// message/rfc822 is body-type-msg in both revisions, so its
		// envelope is still mandatory and is not probed for.
		name: "message/rfc822 (unchanged)",
		data: `("message" "rfc822" NIL NIL NIL "7bit" 6726 (NIL "Fwd" NIL NIL NIL NIL NIL NIL NIL NIL) ("text" "plain" NIL NIL NIL "7bit" 100 4) 123)`,
		parsed: &imap.BodyStructureSinglePart{
			Type:     "message",
			Subtype:  "rfc822",
			Encoding: "7bit",
			Size:     6726,
			MessageRFC822: &imap.BodyStructureMessageRFC822{
				Envelope: &imap.Envelope{Subject: "Fwd"},
				BodyStructure: &imap.BodyStructureSinglePart{
					Type:     "text",
					Subtype:  "plain",
					Encoding: "7bit",
					Size:     100,
					Text:     &imap.BodyStructureText{NumLines: 4},
				},
				NumLines: 123,
			},
		},
	},
	{
		name: "text/plain (unchanged)",
		data: `("text" "html" ("charset" "utf-8") NIL NIL "quoted-printable" 5606 133 NIL NIL NIL NIL)`,
		parsed: &imap.BodyStructureSinglePart{
			Type:     "text",
			Subtype:  "html",
			Params:   map[string]string{"charset": "utf-8"},
			Encoding: "quoted-printable",
			Size:     5606,
			Text:     &imap.BodyStructureText{NumLines: 133},
			Extended: &imap.BodyStructureSinglePartExt{},
		},
	},
}

func TestReadBodyStructure(t *testing.T) {
	for _, tc := range bodyStructureCases {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.data)), imapwire.ConnSideClient)
			bs, err := readBody(dec, &Options{})
			if err != nil {
				t.Fatalf("readBody() = %v", err)
			}
			if !reflect.DeepEqual(bs, tc.parsed) {
				t.Fatalf("readBody() =\n\t%#v\nwant\n\t%#v", bs, tc.parsed)
			}
		})
	}
}

// TestReadBodyStructureDovecotMultipart is the full multipart capture the
// upstream report was filed against: a message/global attachment nested in a
// mixed/related/alternative tree.
func TestReadBodyStructureDovecotMultipart(t *testing.T) {
	const data = `(("text" "plain" ("charset" "UTF-8") NIL NIL "quoted-printable" 576 31 NIL NIL NIL NIL)("message" "global" ("name" "Untitled attachment 00019.dat") NIL NIL "7bit" 6726 NIL ("attachment" ("filename" "Untitled attachment 00019.dat")) NIL NIL) "mixed" ("boundary" "----=_NextPart_000_0061") NIL ("en-us") NIL)`

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

	global, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("Children[1] = %T, want *imap.BodyStructureSinglePart", mp.Children[1])
	}
	if global.Subtype != "global" {
		t.Errorf("Children[1].Subtype = %q, want %q", global.Subtype, "global")
	}
	if global.MessageRFC822 != nil {
		t.Errorf("Children[1].MessageRFC822 = %#v, want nil", global.MessageRFC822)
	}
	if global.Extended == nil || global.Extended.Disposition == nil {
		t.Fatalf("Children[1].Extended.Disposition = nil, want the attachment disposition")
	}
	if got := global.Extended.Disposition.Value; got != "attachment" {
		t.Errorf("Children[1] disposition = %q, want %q", got, "attachment")
	}
	if got := mp.Extended.Language; !reflect.DeepEqual(got, []string{"en-us"}) {
		t.Errorf("Language = %q, want [en-us]", got)
	}
}
