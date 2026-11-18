package imapserver

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// groupHeaders is message 4 of imaptest's fetch-envelope.mbox, plus a Bcc group.
const groupHeaders = "Date: Thu, 15 Feb 2007 01:02:03 +0200\r\n" +
	"From: user@domain (Real Name)\r\n" +
	"To: group: g1@d1.org, g2@d2.org;, group2: g3@d3.org;\r\n" +
	"Cc: group:;, group2: (foo) ;\r\n" +
	"Bcc: Plain <plain@example.com>, \"Team Name\": a@example.com;\r\n"

// checkGroupEnvelope checks the RFC 3501 §7.4.2 group records: a start is an address
// with a NIL host and the group name as its mailbox, an end has both NIL, and an empty
// group is a start directly followed by its end.
func checkGroupEnvelope(t *testing.T, env *imap.Envelope) {
	t.Helper()
	end := imap.Address{}
	for _, tc := range []struct {
		field     string
		got, want []imap.Address
	}{
		{"From", env.From, []imap.Address{{Name: "Real Name", Mailbox: "user", Host: "domain"}}},
		{"To", env.To, []imap.Address{
			{Mailbox: "group"}, {Mailbox: "g1", Host: "d1.org"}, {Mailbox: "g2", Host: "d2.org"}, end,
			{Mailbox: "group2"}, {Mailbox: "g3", Host: "d3.org"}, end,
		}},
		{"Cc", env.Cc, []imap.Address{{Mailbox: "group"}, end, {Mailbox: "group2"}, end}},
		{"Bcc", env.Bcc, []imap.Address{
			{Name: "Plain", Mailbox: "plain", Host: "example.com"},
			{Mailbox: "Team Name"}, {Mailbox: "a", Host: "example.com"}, end,
		}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s = %+v, want %+v", tc.field, tc.got, tc.want)
		}
	}
}

func TestExtractEnvelopeAddressGroups(t *testing.T) {
	header, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(groupHeaders + "\r\n")))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	checkGroupEnvelope(t, ExtractEnvelope(header))
}

// TestExtractBodyStructureAttachedMessageAddressGroups checks the ENVELOPE of an
// attached message/rfc822 part, which comes from ExtractEnvelope.
func TestExtractBodyStructureAttachedMessageAddressGroups(t *testing.T) {
	msg := "Content-Type: multipart/mixed; boundary=b\r\n" +
		"\r\n" +
		"--b\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		groupHeaders +
		"\r\n" +
		"body\r\n" +
		"--b--\r\n"
	mp, ok := ExtractBodyStructure(strings.NewReader(msg)).(*imap.BodyStructureMultiPart)
	if !ok || len(mp.Children) != 1 {
		t.Fatalf("expected a multipart with one child")
	}
	part, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || part.MessageRFC822 == nil || part.MessageRFC822.Envelope == nil {
		t.Fatalf("expected a message/rfc822 part with an envelope, got %#v", mp.Children[0])
	}
	checkGroupEnvelope(t, part.MessageRFC822.Envelope)
}
