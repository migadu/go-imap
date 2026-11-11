package internal

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPreviewChars is the longest preview RFC 8970 §4 allows, in characters
// (Unicode code points, not octets).
const MaxPreviewChars = 256

// SanitizePreview makes s fit the PREVIEW grammar: valid UTF-8 with no NUL or
// other control characters (whitespace controls become a space), at most
// MaxPreviewChars characters long.
func SanitizePreview(s string) string {
	clean := true
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			clean = false
			break
		}
		n++
	}
	if clean && n <= MaxPreviewChars {
		return s
	}

	var b strings.Builder
	n = 0
	for _, r := range strings.ToValidUTF8(s, "") {
		if n == MaxPreviewChars {
			break
		}
		if unicode.IsControl(r) {
			if !unicode.IsSpace(r) {
				continue
			}
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
