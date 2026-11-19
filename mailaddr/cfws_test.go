package mailaddr_test

import (
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/mailaddr"
)

// Only complete RFC 5322 comments are CFWS. Normalizing an unclosed comment must
// not turn an invalid address list into a successful parse of its prefix.
func TestParseListUnterminatedComments(t *testing.T) {
	for _, value := range []string{
		"user@example.com (unterminated",
		"a@example.com, b@example.com (unclosed",
		"<user@example.com> (unterminated",
		"user@example.com (outer (inner)",
		"user@example.com (escaped\\)",
		"user@example.com (dangling\\",
		"user@example.com (complete) (unterminated",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := mailaddr.ParseList(value); err == nil {
				t.Errorf("ParseList(%q) = %+v, nil; want an error", value, got)
			}
		})
	}
}

func TestParseListCompleteAddrSpecComments(t *testing.T) {
	for _, tc := range []struct {
		value, mailbox string
	}{
		{"<user (outer (inner)) @ example.com>", "user"},
		{"<user (escaped \\)) @ example.com>", "user"},
		{"<(before) user (local) @ (domain) example.com>", "user"},
		{`<"user (not a comment)" (local) @ example.com>`, "user (not a comment)"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := mailaddr.ParseList(tc.value)
			want := []imap.Address{{Mailbox: tc.mailbox, Host: "example.com"}}
			if err != nil || !slices.Equal(got, want) {
				t.Errorf("ParseList(%q) = %+v, %v; want %+v, nil", tc.value, got, err, want)
			}
		})
	}
}
