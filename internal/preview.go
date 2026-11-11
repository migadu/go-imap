package internal

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPreviewChars is the longest preview RFC 8970 §3.3 allows, in characters
// (Unicode code points, not octets).
const MaxPreviewChars = 256

// SanitizePreview makes s fit a single-line server PREVIEW value: valid UTF-8
// with no NUL or other control characters (whitespace controls become a space,
// with CRLF treated as a single line break), at most MaxPreviewChars characters
// long.
func SanitizePreview(s string) string {
	clean := true
	n := 0
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && width == 1) || unicode.IsControl(r) {
			clean = false
			break
		}
		i += width
		n++
	}
	if clean && n <= MaxPreviewChars {
		return s
	}

	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n = 0
	for i := 0; i < len(s) && n < MaxPreviewChars; {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		if r == '\r' {
			if i < len(s) && s[i] == '\n' {
				i++
			}
			r = ' '
		} else if unicode.IsControl(r) {
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

// CleanPreview enforces RFC 8970 §3.3's UTF-8 and 256-character limits on a
// preview received by a client, dropping non-whitespace control characters
// while keeping text/plain tabs and line breaks (RFC 8970 §3.2, §3.3, §5).
func CleanPreview(s string) string {
	clean := true
	n := 0
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && width == 1) || isDisallowedClientPreviewControl(r) {
			clean = false
			break
		}
		i += width
		n++
	}
	if clean && n <= MaxPreviewChars {
		return s
	}

	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n = 0
	for i := 0; i < len(s) && n < MaxPreviewChars; {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		if isDisallowedClientPreviewControl(r) {
			continue
		}
		if r == '\r' && n == MaxPreviewChars-1 && hasLeadingLFSkippingDisallowed(s[i:]) {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func isDisallowedClientPreviewControl(r rune) bool {
	return unicode.IsControl(r) && r != '\r' && r != '\n' && r != '\t'
}

func hasLeadingLFSkippingDisallowed(s string) bool {
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		if isDisallowedClientPreviewControl(r) {
			continue
		}
		return r == '\n'
	}
	return false
}
