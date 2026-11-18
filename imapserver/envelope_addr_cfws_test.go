package imapserver

import (
	"slices"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// TestExtractEnvelopeAddrSpecCFWS: RFC 5322 §3.4.1 builds the local-part and the domain
// of an addr-spec from dot-atoms, and a dot-atom may have CFWS (comments and folding
// white space) on either side, so "<user (comment)@ (comment) example.com>" is the
// address user@example.com. net/mail rejects it, and ExtractEnvelope dropped the whole
// header (#381). A list net/mail parses is unchanged, a trailing comment still being the
// display name there.
func TestExtractEnvelopeAddrSpecCFWS(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        []imap.Address
	}{
		{"imaptest search-addresses message 3", "<user-from (comment)@ (comment) domain.org>",
			[]imap.Address{{Mailbox: "user-from", Host: "domain.org"}}},
		{"bare addr-spec with comments", "user-from (comment)@ (comment) domain.org",
			[]imap.Address{{Mailbox: "user-from", Host: "domain.org"}}},
		{"display name kept", "Real Name <user (c) @ example.com>",
			[]imap.Address{{Name: "Real Name", Mailbox: "user", Host: "example.com"}}},
		{"comment inside the angle brackets after the domain", "<user@example.com (c)>",
			[]imap.Address{{Mailbox: "user", Host: "example.com"}}},
		{"nested comment and escaped paren", "<user (a (b) \\) c)@example.com>",
			[]imap.Address{{Mailbox: "user", Host: "example.com"}}},
		{"other addresses of the list kept", "a@example.com, <b (c)@example.org>",
			[]imap.Address{{Mailbox: "a", Host: "example.com"}, {Mailbox: "b", Host: "example.org"}}},
		{"quoted local part keeps its parens and at-sign", `"x (y) @z" (c) @ example.com`,
			[]imap.Address{{Mailbox: "x (y) @z", Host: "example.com"}}},
		{"in a group", "team: inside (comment)@ (comment) example.com;",
			[]imap.Address{{Mailbox: "team"}, {Mailbox: "inside", Host: "example.com"}, {}}},
		{"after a source route", "Real Name < (before) @first (hop) , (between) @second : user (local) @ domain >",
			[]imap.Address{{Name: "Real Name", Mailbox: "user", Host: "domain", Route: "@first,@second"}}},
		{"unchanged: trailing comment is the name", "user-from@domain.org (RealFrom)",
			[]imap.Address{{Name: "RealFrom", Mailbox: "user-from", Host: "domain.org"}}},
		{"unchanged: plain name-addr", "RealFrom <user-from@domain.org>",
			[]imap.Address{{Name: "RealFrom", Mailbox: "user-from", Host: "domain.org"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h textproto.Header
			h.Set("From", tc.value)
			if got := ExtractEnvelope(h).From; !slices.Equal(got, tc.want) {
				t.Errorf("From: %s\n got %+v\nwant %+v", tc.value, got, tc.want)
			}
		})
	}
}
