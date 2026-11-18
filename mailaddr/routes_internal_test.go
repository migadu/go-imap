package mailaddr

import (
	"slices"
	"testing"
)

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
