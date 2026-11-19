package imapserver

import (
	"slices"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// TestExtractEnvelopeInvalidBytes: an address header with bytes that are not text
// (invalid UTF-8, a NUL) still has its addresses. Such bytes are removed before the
// header is parsed, which is what a backend storing the addresses does (migadu/sora's
// top-level ENVELOPE), so a message's own ENVELOPE and its ENVELOPE as an attached
// message/rfc822 agree. ExtractEnvelope parsed the header as written, net/mail rejected
// it, and the field was NIL (#381).
func TestExtractEnvelopeInvalidBytes(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        []imap.Address
	}{
		{"invalid byte in a route hop", "<@route\x80.example:user@domain>", []imap.Address{{Mailbox: "user", Host: "domain", Route: "@route.example"}}},
		{"invalid byte in a route comment", "<(route\xff) @route:user@domain>", []imap.Address{{Mailbox: "user", Host: "domain", Route: "@route"}}},
		{"Latin-1 byte in the local part", "caf\xe9@example.com", []imap.Address{{Mailbox: "caf", Host: "example.com"}}},
		{"invalid byte in a display name", "Na\x80me <user@example.com>, pl\xffain@example.com",
			[]imap.Address{{Name: "Name", Mailbox: "user", Host: "example.com"}, {Mailbox: "plain", Host: "example.com"}}},
		{"NUL in a display name and an address", "Na\x00me <us\x00er@example.com>", []imap.Address{{Name: "Name", Mailbox: "user", Host: "example.com"}}},
		{"invalid byte in a group name", "te\xc0am: a@example.com;", []imap.Address{{Mailbox: "team"}, {Mailbox: "a", Host: "example.com"}, {}}},
		{"a group name of only invalid bytes is no name", "\x96: a@example.com;", nil},
		{"truncated UTF-8 sequence", "J\xc3 <j@example.com>", []imap.Address{{Name: "J", Mailbox: "j", Host: "example.com"}}},
		{"valid U+FFFD is text", "� <j@example.com>", []imap.Address{{Name: "�", Mailbox: "j", Host: "example.com"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h textproto.Header
			h.Set("To", tc.value)
			if got := ExtractEnvelope(h).To; !slices.Equal(got, tc.want) {
				t.Errorf("To: %q\n got %+v\nwant %+v", tc.value, got, tc.want)
			}
		})
	}
}
