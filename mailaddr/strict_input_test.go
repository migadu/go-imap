package mailaddr_test

import (
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/mailaddr"
)

// Strict parsing validates the original value, even when comments would otherwise
// be skipped or decoded display names would be cleaned. Lenient parsing remains
// available for a stored header whose invalid bytes should be removed first.
func TestParseListRejectsInvalidRawText(t *testing.T) {
	for _, encoding := range []struct{ name, bytes string }{
		{"isolated continuation", "\x80"},
		{"truncated", "\xe2\x82"},
		{"overlong", "\xc0\xaf"},
		{"surrogate", "\xed\xa0\x80"},
		{"out of range", "\xf4\x90\x80\x80"},
		{"NUL", "\x00"},
	} {
		for _, context := range []struct{ name, before, after string }{
			{"trailing comment", "user@example.com (na", "me)"},
			{"addr-spec comment", "<user (c", ") @ example.com>"},
			{"group-name comment", "team (c", "): a@example.com;"},
			{"empty group comment", "team: (c", ");"},
			{"comment after group", "team: a@example.com; (c", ")"},
		} {
			t.Run(encoding.name+"/"+context.name, func(t *testing.T) {
				value := context.before + encoding.bytes + context.after
				if got, err := mailaddr.ParseList(value); err == nil || len(got) != 0 {
					t.Errorf("ParseList(%q) = %+v, %v; want no entries and an error", value, got, err)
				}
			})
		}
	}
}

func TestParseListValidRawAndDecodedText(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        []imap.Address
	}{
		{"Unicode comment", "user@example.com (naïve)", []imap.Address{{Name: "naïve", Mailbox: "user", Host: "example.com"}}},
		{"Unicode group name", "équipe (注): user@example.com;", []imap.Address{{Mailbox: "équipe"}, {Mailbox: "user", Host: "example.com"}, {}}},
		{"valid U+FFFD", "\uFFFD <user@example.com>", []imap.Address{{Name: "\uFFFD", Mailbox: "user", Host: "example.com"}}},
		// These values are valid ASCII as written. Only the decoded names need
		// cleaning, which must remain distinct from rejecting malformed raw input.
		{"encoded invalid UTF-8", "=?utf-8?q?ab=80c?= <user@example.com>", []imap.Address{{Name: "abc", Mailbox: "user", Host: "example.com"}}},
		{"encoded NUL", "=?utf-8?q?ab=00c?= <user@example.com>", []imap.Address{{Name: "abc", Mailbox: "user", Host: "example.com"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := mailaddr.ParseList(tc.value); err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ParseList(%q) = %+v, %v; want %+v", tc.value, got, err, tc.want)
			}
		})
	}
	if got, err := mailaddr.ParseListLenient("user@example.com (na\x80me)"); err != nil || !slices.Equal(got, []imap.Address{{Name: "name", Mailbox: "user", Host: "example.com"}}) {
		t.Errorf("ParseListLenient must retain the address after sanitation: %+v, %v", got, err)
	}
}
