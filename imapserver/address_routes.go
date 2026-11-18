package imapserver

import (
	"strings"

	"github.com/emersion/go-message/mail"
)

// parseAddressListRoutes parses an address list as mail.ParseAddressList does, and also
// one whose angle-addrs carry an RFC 5322 §4.4 obs-route ("<@hop1,@hop2:user@domain>"),
// which net/mail rejects. routes is nil when no address has a route; otherwise it has one
// entry per address, the route in canonical form (see addressRoute) or "".
//
// A list that parses without help is returned as it is, so nothing that parsed before
// changes. Otherwise the well-formed routes are cut out and the rest is parsed again. A
// route is kept only when it is certain which address it belongs to: the list must be a
// plain run of mailboxes, one per comma-separated element (parseAddressGroups hands
// group members and the runs between groups over that way). If it is not, the addresses
// are still returned, without routes.
func parseAddressListRoutes(list string) (addrs []*mail.Address, routes []string, err error) {
	addrs, err = mail.ParseAddressList(list)
	if err == nil {
		return addrs, nil, nil
	}
	stripped, found, aligned := stripAddressRoutes(list)
	if stripped == list {
		return nil, nil, err
	}
	addrs, err = mail.ParseAddressList(stripped)
	if err != nil {
		return nil, nil, err
	}
	if !aligned || len(found) != len(addrs) {
		return addrs, nil, nil
	}
	return addrs, found, nil
}

// stripAddressRoutes removes every well-formed obs-route that opens an angle-addr of
// list. routes has the canonical route, or "", of each non-empty comma-separated element
// in order. aligned is false when the list has group syntax at its top level, where an
// element is not one mailbox. Colons in display names, comments, quoted strings and
// domain literals are not route separators. stripped is list itself when there is no
// route to remove.
func stripAddressRoutes(list string) (stripped string, routes []string, aligned bool) {
	var out strings.Builder
	start := 0
	aligned = true
	route, nonBlank := "", false
	endElement := func() {
		if nonBlank {
			routes = append(routes, route)
		}
		route, nonBlank = "", false
	}
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case ' ', '\t', '\r', '\n':
		case ',':
			endElement()
		case '(':
			i = skipComment(list, i)
		case '"':
			nonBlank = true
			i = skipQuotedString(list, i)
		case '[':
			nonBlank = true
			i = skipDomainLiteral(list, i)
		case '<':
			nonBlank = true
			if end, canonical, ok := addressRoute(list, i+1); ok {
				out.WriteString(list[start : i+1])
				start, route = end, canonical
				i = skipAngleAddr(list, end-1)
			} else {
				i = skipAngleAddr(list, i)
			}
		case ':', ';':
			aligned = false
		default:
			nonBlank = true
		}
	}
	endElement()
	if start == 0 {
		return list, nil, aligned
	}
	out.WriteString(list[start:])
	return out.String(), routes, aligned
}

// addressRoute recognizes the obs-route at s[i:], just inside the "<" of an angle-addr
// (RFC 5322 §4.4: obs-route = obs-domain-list ":", obs-domain-list = *(CFWS / ",") "@"
// domain *("," [CFWS] ["@" domain])). end is the index after its ":", where the addr-spec
// starts. route is the at-domain-list an ENVELOPE reports as addr-adl (RFC 9051 §7.5.2),
// in the form Dovecot gives it: every hop as "@" domain, joined by ",", with comments,
// white space and empty list elements dropped. At least one hop is required and every
// hop's domain must be well formed; anything else is not a route.
func addressRoute(s string, i int) (end int, route string, ok bool) {
	var hops strings.Builder
	i, ok = routeCFWSEnd(s, i)
	for ok && i < len(s) && s[i] == ',' {
		i, ok = routeCFWSEnd(s, i+1)
	}
	if !ok || i == len(s) || s[i] != '@' {
		return 0, "", false
	}
	for {
		if hops.Len() > 0 {
			hops.WriteByte(',')
		}
		hops.WriteByte('@')
		i, ok = routeDomainEnd(s, i+1, &hops)
		if !ok || i == len(s) {
			return 0, "", false
		}
		if s[i] != ':' && s[i] != ',' {
			return 0, "", false
		}
		for s[i] == ',' {
			i, ok = routeCFWSEnd(s, i+1)
			if !ok || i == len(s) {
				return 0, "", false
			}
		}
		if s[i] == ':' {
			return i + 1, hops.String(), true
		}
		if s[i] != '@' {
			return 0, "", false
		}
	}
}

// routeDomainEnd validates the domain of a route hop at s[i:] and appends it to hops: a
// dot-atom, without the CFWS obs-domain allows around its dots, or a domain literal as
// written (unfolded). It returns the index after the domain and its trailing CFWS.
func routeDomainEnd(s string, i int, hops *strings.Builder) (int, bool) {
	i, ok := routeCFWSEnd(s, i)
	if !ok || i == len(s) {
		return 0, false
	}
	if s[i] == '[' {
		hops.WriteByte('[')
		for i++; i < len(s); i++ {
			switch ch := s[i]; ch {
			case ']':
				hops.WriteByte(']')
				return routeCFWSEnd(s, i+1)
			case '\\':
				i++
				if i == len(s) || s[i] == 0 || s[i] == '\r' || s[i] == '\n' {
					return 0, false
				}
				hops.WriteByte('\\')
				hops.WriteByte(s[i])
			case '\r':
				if i+2 >= len(s) || s[i+1] != '\n' || (s[i+2] != ' ' && s[i+2] != '\t') {
					return 0, false
				}
				i++
			case '[', '\n', 0:
				return 0, false
			default:
				if ch > 127 {
					return 0, false
				}
				hops.WriteByte(ch)
			}
		}
		return 0, false
	}
	for {
		start := i
		for i < len(s) && isAtext(s[i]) {
			i++
		}
		if start == i {
			return 0, false
		}
		hops.WriteString(s[start:i])
		i, ok = routeCFWSEnd(s, i)
		if !ok || i == len(s) || s[i] != '.' {
			return i, ok
		}
		hops.WriteByte('.')
		i, ok = routeCFWSEnd(s, i+1)
		if !ok {
			return 0, false
		}
	}
}

// isAtext reports whether ch is an RFC 5322 §3.2.3 atext character.
func isAtext(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
		strings.IndexByte("!#$%&'*+-/=?^_`{|}~", ch) >= 0
}

// routeCFWSEnd skips the comments and folding white space at s[i:] and returns the index
// of the next token. It fails on an unterminated comment, a bare CR or LF and a NUL: a
// malformed route must not be cut out as if it were one.
func routeCFWSEnd(s string, i int) (int, bool) {
	depth := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t':
			i++
		case '\r':
			if i+2 >= len(s) || s[i+1] != '\n' || (s[i+2] != ' ' && s[i+2] != '\t') {
				return 0, false
			}
			i += 2
		case '(':
			depth++
			i++
		case ')':
			if depth == 0 {
				return i, true
			}
			depth--
			i++
		case '\\':
			if depth == 0 {
				return i, true
			}
			i++
			if i == len(s) || s[i] == 0 || s[i] == '\r' || s[i] == '\n' {
				return 0, false
			}
			i++
		case '\n', 0:
			return 0, false
		default:
			if depth == 0 {
				return i, true
			}
			i++
		}
	}
	return i, depth == 0
}
