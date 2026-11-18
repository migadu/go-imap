package imapserver

import (
	"slices"
	"testing"

	"github.com/emersion/go-message/textproto"
	"github.com/migadu/go-imap/internal/imapwire"
)

// TestExtractEnvelopeSourceRoute: the second field of an address structure is the
// "at-domain-list (source route and obs-route ABNF production from [RFC5322])"
// (RFC 9051 §7.5.2, RFC 3501 §7.4.2). An angle-addr with an obs-route was dropped whole,
// and the encoder wrote NIL for every addr-adl (#379). The route goes out as Dovecot
// reports it: each hop as "@" domain, joined by ",", without CFWS or empty elements.
func TestExtractEnvelopeSourceRoute(t *testing.T) {
	for _, tc := range []struct{ name, from, want string }{
		{"imaptest fetch-envelope", "<@route:user@domain>", `((NIL "@route" "user" "domain"))`},
		{"two hops", "<@first,@second:user@domain>", `((NIL "@first,@second" "user" "domain"))`},
		{"display name", "Real Name <@route:user@domain>", `(("Real Name" "@route" "user" "domain"))`},
		{"comments and folding", "Real Name <\r\n (before) @first (hop) ,\r\n\t(between) @second :user@domain>", `(("Real Name" "@first,@second" "user" "domain"))`},
		{"comments inside a hop domain", "<@first (c) . (c) example, @second:user@domain>", `((NIL "@first.example,@second" "user" "domain"))`},
		{"empty route elements", "<, (before) , @first,, (between) @second, :user@domain>", `((NIL "@first,@second" "user" "domain"))`},
		{"domain literals", "<@[2001:db8::1],@[192.0.2.1]:user@domain>", `((NIL "@[2001:db8::1],@[192.0.2.1]" "user" "domain"))`},
		{"quoted local part", `<@route:"x:y@z"@domain>`, `((NIL "@route" "x:y@z" "domain"))`},
		{"route text in a quoted display name", `"Name: <@fake:other@domain>" <@route:user@domain>`, `(("Name: <@fake:other@domain>" "@route" "user" "domain"))`},
		{"the route belongs to its own address", "plain@domain, <@first:one@domain>, Two <two@domain>, <@second:three@domain>",
			`((NIL NIL "plain" "domain")(NIL "@first" "one" "domain")("Two" NIL "two" "domain")(NIL "@second" "three" "domain"))`},
		{"in a group", "team: <@first:one@domain>, two@domain;",
			`((NIL NIL "team" NIL)(NIL "@first" "one" "domain")(NIL NIL "two" "domain")(NIL NIL NIL NIL))`},
		{"no route", "Plain <user@domain>", `(("Plain" NIL "user" "domain"))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h textproto.Header
			h.Set("From", tc.from)
			wire := encodeForTest(func(enc *imapwire.Encoder) { writeEnvelope(enc, ExtractEnvelope(h)) })
			// From is copied to Sender and Reply-To when they are absent, route included.
			if want := "(NIL NIL " + tc.want + " " + tc.want + " " + tc.want + " NIL NIL NIL NIL NIL)"; wire != want {
				t.Errorf("ENVELOPE for From: %s\n got %s\nwant %s", tc.from, wire, want)
			}
		})
	}
}

// TestExtractEnvelopeMalformedSourceRoute: only a well-formed obs-route is recognized;
// anything else is still not an address, as before.
func TestExtractEnvelopeMalformedSourceRoute(t *testing.T) {
	for _, from := range []string{
		"<@:user@domain>",
		"<route:user@domain>",
		"<@first,second:user@domain>",
		"<@first..example:user@domain>",
		"<@first words:user@domain>",
		"<@first:>",
		"<@first:other:user@domain>",
		"<@first (unfinished:user@domain>",
		"<@[unfinished:user@domain>",
	} {
		t.Run(from, func(t *testing.T) {
			var h textproto.Header
			h.Set("From", from)
			if env := ExtractEnvelope(h); len(env.From) != 0 {
				t.Errorf("From: %s gave %+v, want no address", from, env.From)
			}
		})
	}
}

// TestParseAddressListRoutesAlignment: a route is reported only for the address it was
// written on. A list that parses without help is untouched, and where the elements are
// not known to be one mailbox each (group syntax in a flat parse) the addresses come
// back without routes rather than with a route on the wrong one.
func TestParseAddressListRoutesAlignment(t *testing.T) {
	for _, tc := range []struct {
		list   string
		addrs  []string
		routes []string
	}{
		{"a@d, B <b@d>", []string{"a@d", "b@d"}, nil},
		{"a@d,, <@r:b@d>, \"q,q\" <c@d>", []string{"a@d", "b@d", "c@d"}, []string{"", "@r", ""}},
		{"<@r1:a@d>,<@r2,@r3:b@d>", []string{"a@d", "b@d"}, []string{"@r1", "@r2,@r3"}},
		{"team: <@r:a@d>, b@d;", []string{"a@d", "b@d"}, nil},
	} {
		addrs, routes, err := parseAddressListRoutes(tc.list)
		if err != nil {
			t.Errorf("parseAddressListRoutes(%q): %v", tc.list, err)
			continue
		}
		var got []string
		for _, a := range addrs {
			got = append(got, a.Address)
		}
		if !slices.Equal(got, tc.addrs) || !slices.Equal(routes, tc.routes) {
			t.Errorf("parseAddressListRoutes(%q) = %q, routes %q; want %q, routes %q", tc.list, got, routes, tc.addrs, tc.routes)
		}
	}
}
