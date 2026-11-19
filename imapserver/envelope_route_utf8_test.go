package imapserver

import (
	"slices"
	"testing"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// RFC 6532 section 3.2 extends atext, dtext and quoted-pair's VCHAR with
// UTF8-non-ascii. A route must retain these characters just as the address does.
func TestExtractEnvelopeUTF8SourceRoute(t *testing.T) {
	routed := func(route string) imap.Address {
		return imap.Address{Mailbox: "user", Host: "domain", Route: route}
	}
	plain := imap.Address{Mailbox: "plain", Host: "domain"}
	for _, tc := range []struct {
		name, value string
		want        []imap.Address
	}{
		{"single hop", "<@routé.example:user@domain>", []imap.Address{routed("@routé.example")}},
		{"mixed list", "plain@domain, <@routé.example:user@domain>, <@ascii:other@domain>", []imap.Address{
			plain, routed("@routé.example"), {Mailbox: "other", Host: "domain", Route: "@ascii"},
		}},
		{"group", "team: <@routé.example:user@domain>, plain@domain;, after@domain", []imap.Address{
			{Mailbox: "team"}, routed("@routé.example"), plain, {}, {Mailbox: "after", Host: "domain"},
		}},
		{"multiple hops", "<@routé.example, @例え.example:user@domain>", []imap.Address{routed("@routé.example,@例え.example")}},
		{"non-BMP", "<@𐐀.example:user@domain>", []imap.Address{routed("@𐐀.example")}},
		{"replacement character", "<@\uFFFD.example:user@domain>", []imap.Address{routed("@\uFFFD.example")}},
		{"domain literal", "<@[例え:route]:user@domain>", []imap.Address{routed("@[例え:route]")}},
		{"quoted pair in literal", "<@[route\\é]:user@domain>", []imap.Address{routed("@[route\\é]")}},
		{"comments", "<(前) @route (café):user@domain>", []imap.Address{routed("@route")}},
		{"quoted pair in comment", "<@route (caf\\é):user@domain>", []imap.Address{routed("@route")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h textproto.Header
			h.Set("To", tc.value)
			if got := ExtractEnvelope(h).To; !slices.Equal(got, tc.want) {
				t.Errorf("To for %q = %+v, want %+v", tc.value, got, tc.want)
			}
		})
	}
}

// A header is read without its bytes that are not text (mailaddr.ParseListLenient), so
// invalid UTF-8 in a source route is removed like anywhere else in an address header
// and the route is what remains: the ENVELOPE of an attached message is the one a
// backend's top-level ENVELOPE has for the same header. Taken as written, such a route is
// not a route (mailaddr's TestParseListMalformedUTF8SourceRoute).
func TestExtractEnvelopeMalformedUTF8SourceRoute(t *testing.T) {
	kept := 0
	for _, encoding := range []struct{ name, bytes string }{
		{"isolated continuation", "\x80"},
		{"truncated", "\xe2\x82"},
		{"overlong", "\xc0\xaf"},
		{"surrogate", "\xed\xa0\x80"},
		{"out of range", "\xf4\x90\x80\x80"},
	} {
		for _, context := range []struct {
			name, before, after string
			want                []imap.Address
		}{
			{"atom", "<@route", ".example:user@domain>", []imap.Address{{Mailbox: "user", Host: "domain", Route: "@route.example"}}},
			{"literal", "<@[route", "]:user@domain>", []imap.Address{{Mailbox: "user", Host: "domain", Route: "@[route]"}}},
			{"leading comment", "<(route", ") @route:user@domain>", []imap.Address{{Mailbox: "user", Host: "domain", Route: "@route"}}},
			{"comment", "<@route (route", "):user@domain>", []imap.Address{{Mailbox: "user", Host: "domain", Route: "@route"}}},
			// Without the byte the backslash escapes the closing bracket or parenthesis.
			{"quoted pair in literal", "<@[route\\", "]:user@domain>", nil},
			{"quoted pair in comment", "<@route (route\\", "):user@domain>", nil},
		} {
			t.Run(encoding.name+"/"+context.name, func(t *testing.T) {
				value := context.before + encoding.bytes + context.after
				var h, clean textproto.Header
				h.Set("To", value)
				clean.Set("To", context.before+context.after)
				got := ExtractEnvelope(h).To
				if !slices.Equal(got, context.want) || !slices.Equal(got, ExtractEnvelope(clean).To) {
					t.Errorf("To: %q = %+v, want %+v, as without the invalid bytes", value, got, context.want)
				}
				kept += len(got)
			})
		}
	}
	if kept != 20 {
		t.Errorf("%d addresses kept, want 20", kept)
	}
}
