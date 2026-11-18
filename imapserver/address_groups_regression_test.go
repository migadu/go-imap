package imapserver

import (
	"slices"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

func TestExtractEnvelopeAddressGroupSyntax(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []imap.Address
	}{
		// RFC 5322 domain literals may contain colons without angle brackets.
		{
			name:  "IPv6 member",
			value: "team: a@[2001:db8::1];",
			want: []imap.Address{
				{Mailbox: "team"},
				{Mailbox: "a", Host: "[2001:db8::1]"},
				{},
			},
		},
		{
			name:  "IPv6 before group",
			value: "a@[2001:db8::1], team: b@example.com;",
			want: []imap.Address{
				{Mailbox: "a", Host: "[2001:db8::1]"},
				{Mailbox: "team"},
				{Mailbox: "b", Host: "example.com"},
				{},
			},
		},
		// RFC 2047 encoded words are decoded only outside quoted strings.
		{
			name:  "quoted encoded-looking word",
			value: `"=?utf-8?q?literal?=": a@example.com;`,
			want: []imap.Address{
				{Mailbox: "=?utf-8?q?literal?="},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "quoted and encoded words",
			value: `"=?utf-8?q?literal?=" =?utf-8?q?real?= : a@example.com;`,
			want: []imap.Address{
				{Mailbox: "=?utf-8?q?literal?= real"},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "adjacent encoded words",
			value: "=?utf-8?q?Caf=C3=A9?= =?utf-8?q?_team?= : a@example.com;",
			want: []imap.Address{
				{Mailbox: "=?utf-8?q?Caf=C3=A9_team?="},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "quoted spaces",
			value: `"two  words": a@example.com;`,
			want: []imap.Address{
				{Mailbox: "two  words"},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "UTF8 spaces are name data",
			value: "\u00a0Team\u00a0: a@example.com;",
			want: []imap.Address{
				{Mailbox: "=?utf-8?q?=C2=A0Team=C2=A0?="},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "comments around phrase",
			value: `(before) "Team Name" (after): a@example.com;`,
			want: []imap.Address{
				{Mailbox: "Team Name"},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		// Each atom or quoted string is a separate word of the phrase.
		{
			name:  "adjacent quoted words",
			value: `"A""B": a@example.com;`,
			want: []imap.Address{
				{Mailbox: "A B"},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "atom before quoted word",
			value: `A"B": a@example.com;`,
			want: []imap.Address{
				{Mailbox: "A B"},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
		{
			name:  "atom after quoted word",
			value: `"A"B: a@example.com;`,
			want: []imap.Address{
				{Mailbox: "A B"},
				{Mailbox: "a", Host: "example.com"},
				{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var header textproto.Header
			header.Set("To", tc.value)
			if got := ExtractEnvelope(header).To; !slices.Equal(got, tc.want) {
				t.Errorf("To = %+v, want %+v", got, tc.want)
			}
		})
	}
}
