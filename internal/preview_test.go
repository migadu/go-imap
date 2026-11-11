package internal

import (
	"strings"
	"testing"
)

func TestSanitizePreview(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "clean", in: "Hello, café!", want: "Hello, café!"},
		{name: "valid replacement rune kept", in: "a\ufffdb", want: "a\ufffdb"},
		{name: "CRLF single space", in: "line1\r\nline2", want: "line1 line2"},
		{name: "controls and CRLF", in: "a\x00b\tc\r\nd\re\nf", want: "ab c d e f"},
		{name: "CRLF counts as one char toward limit", in: strings.Repeat("a", 254) + "\r\nb", want: strings.Repeat("a", 254) + " b"},
		{name: "invalid UTF-8 dropped", in: "Caf\xffé", want: "Café"},
		{name: "code point limit", in: strings.Repeat("é", 300), want: strings.Repeat("é", 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizePreview(tc.in); got != tc.want {
				t.Fatalf("SanitizePreview(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanPreview(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "preserves CRLF and tabs", in: "Line 1\r\n\r\n\tLine 2", want: "Line 1\r\n\r\n\tLine 2"},
		{name: "drops NUL ESC and invalid UTF-8", in: "hi\x00\x1b[31m\xff\tthere\r\n", want: "hi[31m\tthere\r\n"},
		{name: "does not split CRLF at 256 boundary", in: strings.Repeat("é", 255) + "\r\n", want: strings.Repeat("é", 255)},
		{name: "keeps bare CR at 256 when not CRLF", in: strings.Repeat("é", 255) + "\rx", want: strings.Repeat("é", 255) + "\r"},
		{name: "keeps complete CRLF at 256 boundary", in: strings.Repeat("é", 254) + "\r\nx", want: strings.Repeat("é", 254) + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanPreview(tc.in); got != tc.want {
				t.Fatalf("CleanPreview(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
