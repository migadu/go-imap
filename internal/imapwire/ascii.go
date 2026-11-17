package imapwire

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IMAP keywords are case-insensitive over US-ASCII only: "ABNF strings are
// case insensitive and the character set for these strings is US-ASCII" (RFC
// 5234 section 2.3). strings.ToUpper, strings.ToLower and strings.EqualFold
// apply Unicode case mapping instead, which turns non-ASCII look-alikes into
// ASCII letters: U+017F (LATIN SMALL LETTER LONG S) into "S", U+0131 (LATIN
// SMALL LETTER DOTLESS I) into "I", U+0130 (LATIN CAPITAL LETTER I WITH DOT
// ABOVE) into "i" and U+212A (KELVIN SIGN) into "k". Compare protocol tokens
// with the functions below instead.

// ToUpperASCII returns s with its ASCII letters upper-cased. Every other byte,
// including each byte of a multi-byte UTF-8 sequence, is left as it is.
func ToUpperASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if 'a' <= s[i] && s[i] <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				b[j] = foldASCII(b[j])
			}
			return string(b)
		}
	}
	return s
}

// ToLowerASCII returns s with its ASCII letters lower-cased. Every other byte,
// including each byte of a multi-byte UTF-8 sequence, is left as it is.
func ToLowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// EqualFoldASCII reports whether a and b are equal when ASCII letters are
// compared without regard to case.
func EqualFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if foldASCII(a[i]) != foldASCII(b[i]) {
			return false
		}
	}
	return true
}

// IsASCII reports whether every byte of s is 7-bit US-ASCII (< 0x80).
func IsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// IsMIMETokenPart reports whether s is non-empty and free of ASCII/Unicode
// control characters, ASCII/Unicode whitespace, invalid UTF-8, and RFC 2045
// tspecials. Valid non-ASCII graphic UTF-8 runes are permitted so callers can
// distinguish unknown non-ASCII subtypes (such as multipart/dİgest) from tokens
// containing whitespace, CTLs, or delimiters.
func IsMIMETokenPart(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); {
		ch := s[i]
		if ch < utf8.RuneSelf {
			if ch <= ' ' || ch == 0x7f || strings.IndexByte(`()<>@,;:\"/[]?=`, ch) >= 0 {
				return false
			}
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && width == 1) || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
		i += width
	}
	return true
}

// IsMediaTypeToken reports whether s has the form type/subtype where both
// parts satisfy IsMIMETokenPart.
func IsMediaTypeToken(s string) bool {
	lhs, rhs, ok := strings.Cut(s, "/")
	return ok && IsMIMETokenPart(lhs) && IsMIMETokenPart(rhs)
}

// MIMEHeadToken extracts the leading MIME type/subtype or disposition token
// before the first uncommented ';', stripping balanced RFC 5322 comments and
// ASCII linear whitespace while lowercasing ASCII letters only. It returns
// ok=false if comments before ';' are unbalanced.
func MIMEHeadToken(raw string) (string, bool) {
	head := raw
	if strings.ContainsAny(raw, "();") {
		var b strings.Builder
		depth := 0
		for i := 0; i < len(raw); i++ {
			switch c := raw[i]; {
			case depth > 0 && c == '\\':
				i++
				if i >= len(raw) {
					return "", false
				}
			case c == '(':
				depth++
				b.WriteByte(' ')
			case c == ')':
				if depth == 0 {
					return "", false
				}
				depth--
			case depth == 0 && c == ';':
				i = len(raw)
			case depth == 0:
				b.WriteByte(c)
			}
		}
		if depth != 0 {
			return "", false
		}
		head = b.String()
	}
	head = strings.Trim(head, " \t\r\n")
	if lhs, rhs, hasSlash := strings.Cut(head, "/"); hasSlash {
		head = strings.Trim(lhs, " \t\r\n") + "/" + strings.Trim(rhs, " \t\r\n")
	}
	return ToLowerASCII(head), true
}

// CTEMechanism returns the Content-Transfer-Encoding mechanism token with
// balanced RFC 5322 comments and surrounding ASCII whitespace removed. If
// comments are unbalanced, the original value or residual parentheses are
// preserved so callers can treat it as unknown.
func CTEMechanism(v string) string {
	if !strings.ContainsRune(v, '(') {
		return v
	}
	var b strings.Builder
	depth := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case depth > 0 && c == '\\':
			i++
			if i >= len(v) {
				return v
			}
		case c == '(':
			depth++
			b.WriteByte(' ')
		case depth > 0 && c == ')':
			depth--
		case depth == 0:
			b.WriteByte(c)
		}
	}
	if depth != 0 {
		return v
	}
	return strings.Trim(b.String(), " \t\r\n")
}
