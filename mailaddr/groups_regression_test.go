package mailaddr_test

import (
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/mailaddr"
)

func TestParseListWhitespaceGroupNames(t *testing.T) {
	for _, tc := range []struct {
		phrase, name string
	}{
		{`" "" "`, "   "},
		{`" """`, "  "},
		{`""" "`, "  "},
		{`" " =?utf-8?q?_?=`, "   "},
	} {
		t.Run(tc.phrase, func(t *testing.T) {
			value := tc.phrase + ": a@b;"
			got, err := mailaddr.ParseList(value)
			want := []imap.Address{{Mailbox: tc.name}, {Mailbox: "a", Host: "b"}, {}}
			if err != nil || !slices.Equal(got, want) {
				t.Errorf("ParseList(%q) = %+v, %v; want group name %q", value, got, err, tc.name)
			}
		})
	}
}

func TestParseListCommentOnlyReturnsError(t *testing.T) {
	for _, value := range []string{
		"(not an address)",
		"(not: an address)",
		"(not: an address",
		", (not: an address),",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := mailaddr.ParseList(value); err == nil || len(got) != 0 {
				t.Errorf("ParseList(%q) = %+v, %v; want an error and no entries", value, got, err)
			}
		})
	}
}

func TestParseListUnterminatedGroupComments(t *testing.T) {
	for _, value := range []string{
		"g: a@b; (unterminated",
		"g: a@b; (outer (inner)",
		`g: a@b; (escaped \)`,
		"g: (unterminated",
		"g: a@b (unterminated",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := mailaddr.ParseList(value); err == nil || len(got) != 0 {
				t.Errorf("ParseList(%q) = %+v, %v; want an error and no entries", value, got, err)
			}
		})
	}
}
