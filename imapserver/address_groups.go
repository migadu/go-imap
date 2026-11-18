package imapserver

import (
	"mime"
	"strings"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// addressListPart is a run of an address-list header value: plain addresses (group ""),
// or one RFC 5322 group, its decoded name and its members (none for an empty group).
type addressListPart struct {
	group     string
	addresses []*mail.Address
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
		addresses, err := mail.ParseAddressList(list)
		if err != nil {
			return false
		}
		parts = append(parts, addressListPart{addresses: addresses})
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
				addresses, err := mail.ParseAddressList(members)
				if err != nil {
					return nil, false
				}
				part.addresses = addresses
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
// ':' (an obsolete route) and quoted local part are not group syntax.
func skipAngleAddr(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '"':
			j = skipQuotedString(s, j)
		case '>':
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

// decodePhrase returns an RFC 5322 phrase (a group's display-name) as text: comments
// dropped, quoted-strings unquoted, runs of whitespace folded to one space and RFC 2047
// encoded-words decoded, as net/mail does for an address's display name.
func decodePhrase(s string) string {
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
	phrase := strings.Join(words, " ")
	if decoded, err := (&mime.WordDecoder{CharsetReader: gomessage.CharsetReader}).DecodeHeader(phrase); err == nil {
		phrase = decoded
	}
	return phrase
}
