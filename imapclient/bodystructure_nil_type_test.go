package imapclient_test

import (
	"testing"

	"github.com/migadu/go-imap"
)

const (
	nilTypeMultipart = `(("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 1152 23)("TEXT" "HTML" NIL NIL NIL "BASE64" 4554 73) "ALTERNATIVE")`
	// QQ Mail, upstream PR emersion/go-imap#781.
	nilTypeQQ = `(NIL NIL ("CHARSET" "GB2312") NIL NIL NIL 7 NIL NIL NIL)`
)

// A body whose media type and subtype are NIL (QQ Mail) is a single part with
// empty names. The check for NIL must not disturb the multiparts around it:
// upstream's version probed with ExpectNIL, which records an error on every
// multipart, and this fork's decoder stops reading after an error. The QQ part
// sits between two multiparts in one response, and the connection must still
// work afterwards.
func TestBodyStructureNILMediaType(t *testing.T) {
	c := newSelectedCodeServer(t,
		"* 1 FETCH (BODYSTRUCTURE "+nilTypeMultipart+")\r\n"+
			"* 2 FETCH (BODYSTRUCTURE "+nilTypeQQ+")\r\n"+
			"* 3 FETCH (BODYSTRUCTURE "+nilTypeMultipart+")\r\n")

	msgs, err := c.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("Fetch() returned %v messages, want 3", len(msgs))
	}
	for _, i := range []int{0, 2} {
		mp, ok := msgs[i].BodyStructure.(*imap.BodyStructureMultiPart)
		if !ok || mp.Subtype != "ALTERNATIVE" || len(mp.Children) != 2 {
			t.Errorf("message %v: BodyStructure = %#v, want an ALTERNATIVE multipart with 2 children", i+1, msgs[i].BodyStructure)
		}
	}
	sp, ok := msgs[1].BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("message 2: BodyStructure = %T, want *imap.BodyStructureSinglePart", msgs[1].BodyStructure)
	}
	if sp.Type != "" || sp.Subtype != "" || sp.Size != 7 || sp.Params["charset"] != "GB2312" {
		t.Errorf("message 2: BodyStructure = %+v, want empty type/subtype, size 7, charset GB2312", sp)
	}

	if err := c.Noop().Wait(); err != nil {
		t.Errorf("Noop() after the FETCH = %v, want the connection intact", err)
	}
}

// A string followed by NIL stays a multipart that lost its children, whose
// body-fld-param is NIL (upstream #701). That is why a NIL subtype is accepted
// only after a NIL media type: ("TEXT" NIL ...) cannot be told apart from this
// shape when the subtype is read.
func TestBodyStructureChildlessMultipartNILParams(t *testing.T) {
	c := newSelectedCodeServer(t, "* 1 FETCH (BODYSTRUCTURE (\"ALTERNATIVE\" NIL NIL NIL))\r\n")

	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Fetch() returned %v messages, want 1", len(msgs))
	}
	mp, ok := msgs[0].BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok || mp.Subtype != "ALTERNATIVE" || len(mp.Children) != 0 {
		t.Errorf("BodyStructure = %#v, want a childless ALTERNATIVE multipart", msgs[0].BodyStructure)
	}
}

// Only NIL may stand in for the media type. Any other atom is still a
// malformed body, not a single part.
func TestBodyStructureAtomMediaTypeRejected(t *testing.T) {
	c := newSelectedCodeServer(t, "* 1 FETCH (BODYSTRUCTURE (TEXT \"PLAIN\" NIL NIL NIL \"7BIT\" 7 1))\r\n")

	if _, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{},
	}).Collect(); err == nil {
		t.Error("Fetch() = nil, want an error for an atom media type")
	}
}
