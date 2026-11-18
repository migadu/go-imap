package imapserver

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// TestExtractEnvelopeQuotedAtLocalPart checks that an address is split into mailbox and
// host at its last '@' (#373): the addr-spec's domain follows the last one, and a quoted
// local part may hold '@' (RFC 5322 §3.4.1). addr-mailbox is the local part with its
// quoting removed (RFC 3501 §9).
func TestExtractEnvelopeQuotedAtLocalPart(t *testing.T) {
	raw := "From: Alice <\"a@b\"@example.com>\r\n" +
		"To: \"t@1@2\"@example.org, plain@example.net\r\n" +
		"Cc: team: \"c@d\"@example.com;\r\n" +
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
		{"From", env.From, []imap.Address{{Name: "Alice", Mailbox: "a@b", Host: "example.com"}}},
		{"To", env.To, []imap.Address{{Mailbox: "t@1@2", Host: "example.org"}, {Mailbox: "plain", Host: "example.net"}}},
		{"Cc", env.Cc, []imap.Address{{Mailbox: "team"}, {Mailbox: "c@d", Host: "example.com"}, {}}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s = %+v, want %+v", tc.field, tc.got, tc.want)
		}
	}
}
