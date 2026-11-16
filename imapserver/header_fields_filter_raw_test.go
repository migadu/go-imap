package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestExtractBodySectionHeaderFieldsPreserveRaw(t *testing.T) {
	const received = "Received: from first\r\n\tby next\r\n"
	const repeated = "X(a:\t1\r\nx(A: 2\r\n"
	const kept = "KeEp: \tvalue \r\n folded value\r\n"
	const subject = "Subject \t: s\r\n"
	const body = "body\x00\xff\r\nX(a: body content\r\n"
	const header = received + repeated + kept + "Key/s@1: 3\r\n" + subject + "\r\n"
	const msg = header + body
	for _, tc := range []struct {
		name string
		item imap.FetchItemBodySection
		want string
	}{
		{"NOT duplicates", imap.FetchItemBodySection{HeaderFieldsNot: []string{"x(a", "kEY/S@1"}}, received + kept + subject + "\r\n"},
		{"positive folded and tolerated name", imap.FetchItemBodySection{HeaderFields: []string{"keep", "subject"}}, kept + subject + "\r\n"},
		{"NOT no-op unicode", imap.FetchItemBodySection{HeaderFieldsNot: []string{"Keep"}}, header},
		{"positive unicode exactness", imap.FetchItemBodySection{HeaderFields: []string{"Keep"}}, "\r\n"},
		{"combined filters", imap.FetchItemBodySection{HeaderFields: []string{"received", "keep", "X(A"}, HeaderFieldsNot: []string{"x(a"}}, received + kept + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.item.Specifier = imap.PartSpecifierHeader
			if got := imapserver.ExtractBodySection(strings.NewReader(msg), &tc.item); string(got) != tc.want {
				t.Fatalf("filtered header = %q, want %q", got, tc.want)
			}
			// The same filter on a complete section must leave the body untouched.
			tc.item.Specifier = imap.PartSpecifierNone
			if got := imapserver.ExtractBodySection(strings.NewReader(msg), &tc.item); string(got) != tc.want+body {
				t.Fatalf("filtered full message = %q, want %q", got, tc.want+body)
			}
		})
	}
}
