// Package mailaddr parses the address-list header fields of a message (From, Sender,
// Reply-To, To, Cc, Bcc) into the address structures of an IMAP ENVELOPE.
//
// It is the one parser behind imapserver.ExtractEnvelope, exported so that a backend
// which stores a message's addresses itself (for its top-level ENVELOPE, for SEARCH and
// SORT) reads a header exactly as the envelope of an attached message does.
package mailaddr

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/mail"

	"github.com/migadu/go-imap"
)

// Sanitize returns a header value without the bytes that are not text: every byte that
// is not part of a valid UTF-8 sequence, and every NUL. Valid text, U+FFFD included, is
// returned as it is. Mail in the wild has such bytes (an unencoded Latin-1 name, a
// truncated sequence), and neither an ENVELOPE nor a database can carry them.
func Sanitize(value string) string {
	return cleanText(value)
}

// ParseListLenient is ParseList(Sanitize(value)): the parse of an address header of a
// stored message, whose addresses are worth keeping when a stray byte is not text.
// imapserver.ExtractEnvelope uses it for the ENVELOPE of an attached message, and a
// backend that stores a message's addresses uses it for the same header, so both read
// every header alike, whatever is in it.
func ParseListLenient(value string) ([]imap.Address, error) {
	return ParseList(Sanitize(value))
}

// ParseList parses the value of one address header field and returns its entries in
// header order, as RFC 9051 section 7.5.2 (RFC 3501 section 7.4.2) lists them in an
// ENVELOPE:
//
//   - an address has its display name in Name, decoded (RFC 2047 encoded-words, quoting
//     and comments resolved as go-message's mail.ParseAddressList does), its local part
//     in Mailbox and its domain in Host. The local part is unquoted, so it may hold an
//     '@' ("a@b"@example.com); the domain follows the last one. Route is its obsolete
//     source route, "@first,@second" for "<@first,@second:user@example.com>", and empty
//     for nearly every address;
//   - an RFC 5322 section 3.4 group is a start entry, with only Mailbox set, to the
//     group's decoded name (Address.IsGroupStart), then its members, then an end entry
//     with every field empty (Address.IsGroupEnd). An empty group, such as
//     "undisclosed-recipients:;", is its start and end.
//
// Every string is the decoded text. Whoever writes an ENVELOPE encodes it: imapserver's
// encoder RFC 2047 encodes Name, and ExtractEnvelope does the same to a group's name.
//
// Besides what net/mail accepts, three things RFC 5322 allows and it rejects are parsed:
// the group syntax itself, which it flattens (an empty group leaves nothing); an obsolete
// source route in an angle-addr (section 4.4), whose hop domains may be internationalized
// (RFC 6532 section 3.2); and comments or white space around the dot-atoms of an addr-spec
// (section 3.4.1), "<user (c) @ (c) example.com>". A valid UTF-8, NUL-free value that
// net/mail parses gives the addresses net/mail gives.
//
// A value that is not well-formed group syntax (a nested or unterminated group, ";"
// outside a group, a group without a name, a run that does not parse) is parsed as a
// flat list, and a value that is not an address list at all is an error. A header field
// that repeats is the caller's to combine, one call per instance.
//
// The original value must contain valid UTF-8 and no NUL, including in comments.
// Invalid UTF-8 or NUL produced by encoded-word decoding is removed from decoded
// names. Use ParseListLenient for a stored header whose invalid bytes should be
// removed before parsing.
func ParseList(value string) ([]imap.Address, error) {
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return nil, errors.New("mailaddr: address list contains invalid UTF-8 or NUL")
	}
	var entries []imap.Address
	add := func(addrs []*mail.Address, routes []string) {
		for i, addr := range addrs {
			// net/mail unquotes the local part, which may hold '@' ("a@b"@example.com),
			// and rejects '@' in the domain, so the domain follows the last '@'.
			at := strings.LastIndexByte(addr.Address, '@')
			if at < 0 {
				continue
			}
			entry := imap.Address{
				Name:    cleanText(addr.Name),
				Mailbox: addr.Address[:at],
				Host:    addr.Address[at+1:],
			}
			if routes != nil {
				entry.Route = routes[i]
			}
			entries = append(entries, entry)
		}
	}
	if parts, ok := parseAddressGroups(value); ok {
		for _, part := range parts {
			if part.group != "" {
				entries = append(entries, imap.Address{Mailbox: part.group})
			}
			add(part.addresses, part.routes)
			if part.group != "" {
				entries = append(entries, imap.Address{})
			}
		}
		return entries, nil
	}
	addrs, routes, err := parseAddressListRoutes(value)
	if err != nil {
		return nil, err
	}
	add(addrs, routes)
	return entries, nil
}
