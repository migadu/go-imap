package mailaddr_test

import (
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/mailaddr"
)

func TestParseListLenientUnclosedComments(t *testing.T) {
	user := imap.Address{Mailbox: "user", Host: "example.com"}
	for _, tc := range []struct {
		name, value string
		want        []imap.Address
	}{
		{"single address", "user@example.com (unterminated", []imap.Address{user}},
		{"mixed list", "a@example.com, b@example.com (unclosed", []imap.Address{{Mailbox: "a", Host: "example.com"}, {Mailbox: "b", Host: "example.com"}}},
		{"group", "team: a@example.com; (unterminated", []imap.Address{{Mailbox: "team"}, {Mailbox: "a", Host: "example.com"}, {}}},
		{"nested comment", "user@example.com (outer (inner)", []imap.Address{user}},
		{"escaped closing parenthesis", `user@example.com (escaped \)`, []imap.Address{user}},
		{"dangling escape", `user@example.com (dangling\`, []imap.Address{user}},
		{"complete comment before unclosed comment", "user@example.com (Actual Name) (unclosed", []imap.Address{{Name: "Actual Name", Mailbox: "user", Host: "example.com"}}},
		{"parenthesis in quoted display name", `"A (team)" <user@example.com> (unclosed`, []imap.Address{{Name: "A (team)", Mailbox: "user", Host: "example.com"}}},
		{"parenthesis in quoted local part", `"user (team)"@example.com (unclosed`, []imap.Address{{Mailbox: "user (team)", Host: "example.com"}}},
		{"parenthesis in route domain literal", "<@[hop(route]:user@example.com> (unclosed", []imap.Address{{Mailbox: "user", Host: "example.com", Route: "@[hop(route]"}}},
		{"invalid bytes removed first", "us\x80er@example.com (unclosed\x00", []imap.Address{user}},
		{"addresses inside the comment are not recipients", "user@example.com (unclosed, fake@example.net", []imap.Address{user}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := mailaddr.ParseList(tc.value); err == nil || len(got) != 0 {
				t.Errorf("strict ParseList(%q) = %+v, %v; want an error", tc.value, got, err)
			}
			if got, err := mailaddr.ParseListLenient(tc.value); err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ParseListLenient(%q) = %+v, %v; want %+v", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestParseListLenientCompleteCommentSyntax(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []imap.Address
	}{
		{"user@example.com (Actual Name)", []imap.Address{{Name: "Actual Name", Mailbox: "user", Host: "example.com"}}},
		{`"A (team)" <user@example.com>`, []imap.Address{{Name: "A (team)", Mailbox: "user", Host: "example.com"}}},
		{`"user (team)"@example.com`, []imap.Address{{Mailbox: "user (team)", Host: "example.com"}}},
		{"<@[hop(route]:user@example.com>", []imap.Address{{Mailbox: "user", Host: "example.com", Route: "@[hop(route]"}}},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got, err := mailaddr.ParseListLenient(tc.value); err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ParseListLenient(%q) = %+v, %v; want %+v", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestParseListLenientDoesNotInventCommentRecipients(t *testing.T) {
	for _, value := range []string{
		"(unclosed user@example.com",
		", (unclosed user@example.com",
		"<user@example.com (unclosed",
		"team: a@example.com (unclosed",
		`"display (not a comment <user@example.com>`,
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := mailaddr.ParseListLenient(value); err == nil || len(got) != 0 {
				t.Errorf("ParseListLenient(%q) = %+v, %v; want an error without inventing an address", value, got, err)
			}
		})
	}
}
