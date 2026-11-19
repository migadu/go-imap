package imapserver

import (
	"slices"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// A stored message's ENVELOPE keeps the complete addresses before an unclosed
// trailing comment, using the same lenient parser as a backend that stores them.
func TestExtractEnvelopeLenientUnclosedComments(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        []imap.Address
	}{
		{"single address", "user@example.com (unterminated", []imap.Address{{Mailbox: "user", Host: "example.com"}}},
		{"mixed list", "a@example.com, b@example.com (unclosed", []imap.Address{{Mailbox: "a", Host: "example.com"}, {Mailbox: "b", Host: "example.com"}}},
		{"group", "team: a@example.com; (unterminated", []imap.Address{{Mailbox: "team"}, {Mailbox: "a", Host: "example.com"}, {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h textproto.Header
			h.Set("From", tc.value)
			h.Set("To", tc.value)
			env := ExtractEnvelope(h)
			if !slices.Equal(env.From, tc.want) || !slices.Equal(env.To, tc.want) {
				t.Errorf("From = %+v, To = %+v; want %+v", env.From, env.To, tc.want)
			}
		})
	}
}
