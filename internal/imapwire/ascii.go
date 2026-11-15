package imapwire

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
