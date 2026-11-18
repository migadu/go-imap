package imapserver

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// TestExtractEnvelopeRepeatedAddressHeader checks that every instance of a repeated
// address header contributes to the envelope list, in header order (#374). RFC 5322 §3.6
// allows one To: field, but mail with two exists; migadu/sora's top-level ENVELOPE
// combines them, and an attached copy of the same message must agree. An instance that
// does not parse adds nothing and does not drop the others.
func TestExtractEnvelopeRepeatedAddressHeader(t *testing.T) {
	raw := "From: user@example.com\r\n" +
		"To: first@example.com\r\n" +
		"Cc: team: c1@example.com;\r\n" +
		"To: second@example.com, team: third@example.com;\r\n" +
		"Cc: not an address\r\n" +
		"Cc: c2@example.com\r\n" +
		"\r\n"
	header, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	env := ExtractEnvelope(header)
	for _, tc := range []struct {
		field     string
		got, want []imap.Address
	}{
		{"To", env.To, []imap.Address{
			{Mailbox: "first", Host: "example.com"},
			{Mailbox: "second", Host: "example.com"},
			{Mailbox: "team"}, {Mailbox: "third", Host: "example.com"}, {},
		}},
		{"Cc", env.Cc, []imap.Address{
			{Mailbox: "team"}, {Mailbox: "c1", Host: "example.com"}, {},
			{Mailbox: "c2", Host: "example.com"},
		}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s = %+v, want %+v", tc.field, tc.got, tc.want)
		}
	}
}
