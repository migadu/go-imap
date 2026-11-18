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

// Invalid UTF-8 must not become a valid address by stripping it out with an
// obs-route, even when it is in a discarded comment or follows a backslash.
func TestExtractEnvelopeMalformedUTF8SourceRoute(t *testing.T) {
	for _, encoding := range []struct{ name, bytes string }{
		{"isolated continuation", "\x80"},
		{"truncated", "\xe2\x82"},
		{"overlong", "\xc0\xaf"},
		{"surrogate", "\xed\xa0\x80"},
		{"out of range", "\xf4\x90\x80\x80"},
	} {
		for _, context := range []struct{ name, before, after string }{
			{"atom", "<@route", ".example:user@domain>"},
			{"literal", "<@[route", "]:user@domain>"},
			{"leading comment", "<(route", ") @route:user@domain>"},
			{"comment", "<@route (route", "):user@domain>"},
			{"quoted pair in literal", "<@[route\\", "]:user@domain>"},
			{"quoted pair in comment", "<@route (route\\", "):user@domain>"},
		} {
			t.Run(encoding.name+"/"+context.name, func(t *testing.T) {
				value := context.before + encoding.bytes + context.after
				var h textproto.Header
				h.Set("To", value)
				if got := ExtractEnvelope(h).To; len(got) != 0 {
					t.Errorf("malformed UTF-8 in %q gave %+v, want no address", value, got)
				}
			})
		}
	}
}
