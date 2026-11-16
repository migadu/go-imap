package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// A field-name is any printable US-ASCII but ":" (RFC 5322 §3.6.8), so it may
// hold bytes such as "(", "/" or "@" that are not HTTP token characters.
// HEADER.FIELDS.NOT must remove such a field in every ASCII case variant, and
// only those (RFC 9051 §6.4.5), just as HEADER.FIELDS keeps it.
func TestExtractBodySectionHeaderFieldsNotNonTokenName(t *testing.T) {
	const msg = "From: a@example.com\r\nX(a: 1\r\nx(A: 2\r\nKey/s@1: 3\r\nSubject: s\r\n\r\nbody\r\n"
	for _, tc := range []struct {
		fieldsNot []string
		want      string
	}{
		{[]string{"x(a"}, "From: a@example.com\r\nKey/s@1: 3\r\nSubject: s\r\n\r\n"},
		{[]string{"X(A", "kEY/S@1"}, "From: a@example.com\r\nSubject: s\r\n\r\n"},
		{[]string{"SUBJECT"}, "From: a@example.com\r\nX(a: 1\r\nx(A: 2\r\nKey/s@1: 3\r\n\r\n"},
		// U+212A KELVIN SIGN lowers to "k" under Unicode case mapping only.
		{[]string{"Key/s@1"}, "From: a@example.com\r\nX(a: 1\r\nx(A: 2\r\nKey/s@1: 3\r\nSubject: s\r\n\r\n"},
	} {
		got := imapserver.ExtractBodySection(strings.NewReader(msg), &imap.FetchItemBodySection{
			Specifier:       imap.PartSpecifierHeader,
			HeaderFieldsNot: tc.fieldsNot,
		})
		if string(got) != tc.want {
			t.Errorf("HEADER.FIELDS.NOT %q = %q, want %q", tc.fieldsNot, got, tc.want)
		}
	}
}
