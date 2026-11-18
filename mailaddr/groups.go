package mailaddr

import (
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// addressListPart is a run of an address-list header value: plain addresses (group ""),
// or one RFC 5322 group, its decoded name and its members (none for an empty group).
type addressListPart struct {
	group     string
	addresses []*mail.Address
	routes    []string // nil, or the obs-route of each address (parseAddressListRoutes)
}

// parseAddressGroups parses an address-list header value that holds RFC 5322 §3.4
// groups (display-name ":" [group-list] ";"), keeping each group's name and bounds,
// which mail.ParseAddressList drops: it flattens the members into the list and an empty
// group leaves nothing at all. ok is false for a value without a group, and for one
// that is not well formed (a nested or unterminated group, ";" outside a group, a group
// without a name, a run that does not parse); the caller then parses it flat.
func parseAddressGroups(value string) (parts []addressListPart, ok bool) {
	if strings.IndexByte(value, ':') < 0 {
		return nil, false
	}
	addPlain := func(list string) bool {
		if isBlankAddressList(list) {
			return true
		}
		addresses, routes, err := parseAddressListRoutes(list)
		if err != nil {
			return false
		}
		parts = append(parts, addressListPart{addresses: addresses, routes: routes})
		return true
	}
	segStart, lastComma := 0, -1
	inGroup := false
	groupName := ""
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			i = skipQuotedString(value, i)
		case '(':
			i = skipComment(value, i)
		case '<':
			i = skipAngleAddr(value, i)
		case '[':
			i = skipDomainLiteral(value, i)
		case ',':
			if !inGroup {
				lastComma = i
			}
		case ':':
			if inGroup {
				return nil, false
			}
			nameStart := segStart
			if lastComma >= segStart {
				if !addPlain(value[segStart:lastComma]) {
					return nil, false
				}
				nameStart = lastComma + 1
			}
			groupName = decodePhrase(value[nameStart:i])
			if groupName == "" {
				return nil, false
			}
			inGroup, segStart = true, i+1
		case ';':
			if !inGroup {
				return nil, false
			}
			part := addressListPart{group: groupName}
			if members := value[segStart:i]; !isBlankAddressList(members) {
				addresses, routes, err := parseAddressListRoutes(members)
				if err != nil {
					return nil, false
				}
				part.addresses, part.routes = addresses, routes
			}
			parts = append(parts, part)
			inGroup, segStart, lastComma = false, i+1, -1
		}
	}
	if inGroup || !addPlain(value[segStart:]) {
		return nil, false
	}
	return parts, true
}

// skipQuotedString returns the index of the '"' closing the quoted-string opened at i
// (the last index if it is unterminated).
func skipQuotedString(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(s) - 1
}

// skipComment returns the index of the ')' closing the (nestable) comment opened at i.
func skipComment(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return len(s) - 1
}

// skipAngleAddr returns the index of the '>' closing the angle-addr opened at i. Its
// ':' (an obsolete route) and quoted local part are not group syntax, and a '>' in one
// of its comments or domain literals does not close it.
func skipAngleAddr(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '"':
			j = skipQuotedString(s, j)
		case '(':
			j = skipComment(s, j)
		case '[':
			j = skipDomainLiteral(s, j)
		case '>':
			return j
		}
	}
	return len(s) - 1
}

// skipDomainLiteral skips the domain-literal opened at i, whose colons (for
// example in an IPv6 address) are not group delimiters.
func skipDomainLiteral(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case ']':
			return j
		}
	}
	return len(s) - 1
}

// isBlankAddressList reports whether s holds no address: only whitespace, comments and
// the empty list elements RFC 5322 §4.4 obs-addr-list allows (bare commas).
func isBlankAddressList(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', ',':
		case '(':
			i = skipComment(s, i)
		default:
			return false
		}
	}
	return true
}

// decodePhrase decodes a group's name. A well-formed phrase is decoded by the address
// parser's own display-name rules, so a group's name reads as an address's does: quoted
// text is kept as it is, RFC 2047 encoded-words are decoded only outside quotes, and each
// atom or quoted string is a word of its own. A name that is not a phrase (an unquoted
// "@" or other special in it) is still the name of the group the ":" opens; it is read
// leniently (decodeLoosePhrase) instead of costing the header its addresses. A name
// without a word, only comments and empty quoted strings, is no name ("").
func decodePhrase(s string) string {
	words := phraseWords(s)
	if len(words) == 0 {
		return ""
	}
	// net/mail accepts comments between words, but not before the first word.
	s = strings.Trim(s, " \t\r\n")
	for len(s) > 0 && s[0] == '(' {
		s = strings.Trim(s[skipComment(s, 0)+1:], " \t\r\n")
	}
	const mailbox = "group-name@example.invalid"
	addr, err := mail.ParseAddress(s + " <" + mailbox + ">")
	if err != nil || addr.Address != mailbox {
		return decodeLoosePhrase(s, words)
	}
	if name := cleanText(addr.Name); strings.Trim(name, " \t\r\n") != "" {
		return name
	}
	// Words, but no text from them (quoted white space, encoded-words that decode to
	// nothing or not at all): the words are the name.
	return decodeLoosePhrase(s, words)
}

// phraseWords splits text that should be an RFC 5322 phrase into its words: comments
// dropped, quoted-strings unquoted, white space separating. Empty quoted strings are not
// words.
func phraseWords(s string) []string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\r', '\n':
			flush()
		case '(':
			flush()
			i = skipComment(s, i)
		case '"':
			end := skipQuotedString(s, i)
			content := s[i+1:]
			if s[end] == '"' && end > i {
				content = s[i+1 : end]
			}
			for j := 0; j < len(content); j++ {
				if content[j] == '\\' && j+1 < len(content) {
					j++
				}
				word.WriteByte(content[j])
			}
			i = end
		default:
			word.WriteByte(c)
		}
	}
	flush()
	return words
}

// decodeLoosePhrase reads text that is not an RFC 5322 phrase as one: its words
// (phraseWords) joined by one space, RFC 2047 encoded-words decoded. Bytes that are not
// text as written (invalid UTF-8, a NUL) have no reading.
func decodeLoosePhrase(s string, words []string) string {
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return "" // no group, as net/mail has no address with such a name
	}
	phrase := strings.Join(words, " ")
	if decoded, err := (&mime.WordDecoder{CharsetReader: message.CharsetReader}).DecodeHeader(phrase); err == nil {
		phrase = decoded
	}
	return cleanText(phrase)
}

// cleanText returns decoded text without what is not text: an encoded-word can decode to
// invalid UTF-8 or to a NUL, which neither an ENVELOPE nor a database can carry.
func cleanText(s string) string {
	if utf8.ValidString(s) && strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == 0 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}
