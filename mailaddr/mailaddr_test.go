package mailaddr_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/mailaddr"
)

func addr(name, mailbox, host string) imap.Address {
	return imap.Address{Name: name, Mailbox: mailbox, Host: host}
}
func routed(route, name, mailbox, host string) imap.Address {
	return imap.Address{Name: name, Mailbox: mailbox, Host: host, Route: route}
}
func group(name string) imap.Address { return imap.Address{Mailbox: name} }

var groupEnd = imap.Address{}

type listCase struct {
	name, value string
	want        []imap.Address
}

func runListCases(t *testing.T, cases []listCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mailaddr.ParseList(tc.value)
			if err != nil {
				t.Fatalf("ParseList(%q): %v", tc.value, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseList(%q)\n got %+v\nwant %+v", tc.value, got, tc.want)
			}
		})
	}
}

// RFC 5322 §3.4 groups keep their name and bounds: a start entry with the decoded name
// as its mailbox, the members, an end entry (RFC 3501 §7.4.2); an empty group is a start
// directly followed by its end.
func TestParseListGroups(t *testing.T) {
	runListCases(t, []listCase{
		// imaptest fetch-envelope.mbox, message 4.
		{"two groups", "group: g1@d1.org, g2@d2.org;, group2: g3@d3.org;", []imap.Address{
			group("group"), addr("", "g1", "d1.org"), addr("", "g2", "d2.org"), groupEnd,
			group("group2"), addr("", "g3", "d3.org"), groupEnd,
		}},
		{"empty groups, one holding only a comment", "group:;, group2: (foo) ;",
			[]imap.Address{group("group"), groupEnd, group("group2"), groupEnd}},
		{"undisclosed-recipients", "undisclosed-recipients:;", []imap.Address{group("undisclosed-recipients"), groupEnd}},
		{"addresses around a group", "Plain <plain@example.com>, team: a@example.com, B <b@example.com>;, last@example.com", []imap.Address{
			addr("Plain", "plain", "example.com"),
			group("team"), addr("", "a", "example.com"), addr("B", "b", "example.com"), groupEnd,
			addr("", "last", "example.com"),
		}},
		{"quoted, commented and encoded group names", `"Team: A" (x) :a@example.com;, =?utf-8?q?Caf=C3=A9?= list:;`, []imap.Address{
			group("Team: A"), addr("", "a", "example.com"), groupEnd,
			group("Café list"), groupEnd,
		}},
		{"colon in a display name or a comment is no group", `"Re: x" <a@example.com>, b@example.com (c: d)`,
			[]imap.Address{addr("Re: x", "a", "example.com"), addr("c: d", "b", "example.com")}},
		{"an address in two groups is in both", "g: a@example.com;, h: a@example.com;", []imap.Address{
			group("g"), addr("", "a", "example.com"), groupEnd,
			group("h"), addr("", "a", "example.com"), groupEnd,
		}},
		{"a repeated address is listed each time", "a@example.com, A <a@example.com>",
			[]imap.Address{addr("", "a", "example.com"), addr("A", "a", "example.com")}},
	})
}

// A group's name is decoded as an address's display name is: quoted text stays as it is,
// RFC 2047 encoded-words are decoded only outside quotes, every atom or quoted string is
// a word. A name that is not a phrase is read leniently rather than losing the group,
// and a name without a word is no name.
func TestParseListGroupNames(t *testing.T) {
	member := addr("", "a", "example.com")
	named := func(name string) []imap.Address { return []imap.Address{group(name), member, groupEnd} }
	runListCases(t, []listCase{
		{"quoted encoded-looking word", `"=?utf-8?q?literal?=": a@example.com;`, named("=?utf-8?q?literal?=")},
		{"quoted and encoded words", `"=?utf-8?q?literal?=" =?utf-8?q?real?= : a@example.com;`, named("=?utf-8?q?literal?= real")},
		{"adjacent encoded words", "=?utf-8?q?Caf=C3=A9?= =?utf-8?q?_team?= : a@example.com;", named("Café team")},
		{"quoted spaces", `"two  words": a@example.com;`, named("two  words")},
		{"UTF-8 spaces are name data", " Team : a@example.com;", named(" Team ")},
		{"comments around the phrase", `(before) "Team Name" (after): a@example.com;`, named("Team Name")},
		{"adjacent quoted words", `"A""B": a@example.com;`, named("A B")},
		{"atom before a quoted word", `A"B": a@example.com;`, named("A B")},
		{"atom after a quoted word", `"A"B: a@example.com;`, named("A B")},
		{"name with a dot", "Mr. X: a@example.com;", named("Mr. X")},
		{"an at-sign in the name", `"b@z": a@example.com;`, named("b@z")},
		// Not phrases: read leniently.
		{"unquoted at-sign", "a@b: a@example.com;", named("a@b")},
		{"only quoted white space", `"  ": a@example.com;`, named("  ")},
		{"encoded-words that do not decode", "=?utf8?B??= =?0?B??=: a@example.com;", named("=?utf8?B??= =?0?B??=")},
	})
	// No word: no name, no group; the value is parsed flat, and here is no address list.
	for _, value := range []string{`"""":;`, `"" (c) "": a@example.com;`, "=?utf-8?q??=: a@example.com;"} {
		if got, err := mailaddr.ParseList(value); err == nil && slices.ContainsFunc(got, func(a imap.Address) bool { return a.IsGroupStart() || a.IsGroupEnd() }) {
			t.Errorf("ParseList(%q) = %+v, want no group", value, got)
		}
	}
}

// A value that is not well-formed group syntax keeps the flat parse mail.ParseAddressList
// gives it, without group entries.
func TestParseListMalformedGroupsParseFlat(t *testing.T) {
	for _, value := range []string{
		"g: a@example.com",                  // unterminated
		"g: h: a@example.com;;",             // nested
		"a@example.com; b@example.com",      // ';' outside a group
		": a@example.com;",                  // no group name
		"g: not an address;, b@example.com", // a member that does not parse
	} {
		t.Run(value, func(t *testing.T) {
			var want []imap.Address
			flat, flatErr := mail.ParseAddressList(value)
			for _, a := range flat {
				at := strings.LastIndexByte(a.Address, '@')
				want = append(want, addr(a.Name, a.Address[:at], a.Address[at+1:]))
			}
			got, err := mailaddr.ParseList(value)
			if (err != nil) != (flatErr != nil) || !slices.Equal(got, want) {
				t.Errorf("ParseList(%q) = %+v, %v; want the flat parse %+v, %v", value, got, err, want, flatErr)
			}
		})
	}
}

// Colons in a bare IPv6 domain literal are part of its address, also when that address
// precedes or belongs to a group. An escaped closing bracket or an unterminated literal
// must not turn the following colon into a group delimiter.
func TestParseListDomainLiterals(t *testing.T) {
	runListCases(t, []listCase{
		{"before a group", "outside@[2001:db8::1], team: inside@example.com;", []imap.Address{
			addr("", "outside", "[2001:db8::1]"), group("team"), addr("", "inside", "example.com"), groupEnd,
		}},
		{"in a group", "team: inside@[2001:db8::1];", []imap.Address{group("team"), addr("", "inside", "[2001:db8::1]"), groupEnd}},
		{"mixed", `outside@[::1] (a comment:;), "A:B": <inside@[2001:db8::2]>;, tail@[::3]`, []imap.Address{
			addr("a comment:;", "outside", "[::1]"), group("A:B"), addr("", "inside", "[2001:db8::2]"), groupEnd, addr("", "tail", "[::3]"),
		}},
	})
	for _, value := range []string{
		`outside@[2001:db8::1, team: inside@example.com;`,
		`outside@[2001:db8::1\], team: inside@example.com;`,
	} {
		if got, err := mailaddr.ParseList(value); err == nil {
			t.Errorf("ParseList(%q) = %+v, want an error", value, got)
		}
	}
}

// RFC 5322 §3.4.1: the dot-atoms of an addr-spec may have comments and folding white
// space around them, which net/mail rejects. A list net/mail parses is unchanged, a
// trailing comment still being the display name there.
func TestParseListAddrSpecCFWS(t *testing.T) {
	runListCases(t, []listCase{
		{"imaptest search-addresses message 3", "<user-from (comment)@ (comment) domain.org>", []imap.Address{addr("", "user-from", "domain.org")}},
		{"bare addr-spec with comments", "user-from (comment)@ (comment) domain.org", []imap.Address{addr("", "user-from", "domain.org")}},
		{"display name kept", "Real Name <user (c) @ example.com>", []imap.Address{addr("Real Name", "user", "example.com")}},
		{"comment inside the angle brackets after the domain", "<user@example.com (c)>", []imap.Address{addr("", "user", "example.com")}},
		{"nested comment and escaped paren", "<user (a (b) \\) c)@example.com>", []imap.Address{addr("", "user", "example.com")}},
		{"other addresses of the list kept", "a@example.com, <b (c)@example.org>", []imap.Address{addr("", "a", "example.com"), addr("", "b", "example.org")}},
		{"quoted local part keeps its parens and at-sign", `"x (y) @z" (c) @ example.com`, []imap.Address{addr("", "x (y) @z", "example.com")}},
		{"in a group", "team: inside (comment)@ (comment) example.com;", []imap.Address{group("team"), addr("", "inside", "example.com"), groupEnd}},
		{"unchanged: trailing comment is the name", "user-from@domain.org (RealFrom)", []imap.Address{addr("RealFrom", "user-from", "domain.org")}},
		{"unchanged: plain name-addr", "RealFrom <user-from@domain.org>", []imap.Address{addr("RealFrom", "user-from", "domain.org")}},
	})
}

// The local part is unquoted and may hold an '@'; the domain follows the last one.
func TestParseListQuotedLocalPart(t *testing.T) {
	runListCases(t, []listCase{
		{"quoted at-sign", `"a@b"@example.com`, []imap.Address{addr("", "a@b", "example.com")}},
		{"quoted at-sign with a name", `Name <"a@b"@example.com>`, []imap.Address{addr("Name", "a@b", "example.com")}},
		{"quoted quote", `"q\"uo"@example.com`, []imap.Address{addr("", `q"uo`, "example.com")}},
		{"encoded display name is decoded", "=?utf-8?q?Caf=C3=A9?= <a@example.com>", []imap.Address{addr("Café", "a", "example.com")}},
		{"internationalized domain", "user@bücher.example", []imap.Address{addr("", "user", "bücher.example")}},
	})
}

// RFC 5322 §4.4 obs-route: the address is kept and its route reported in canonical form,
// every hop as "@" domain joined by ",", without CFWS or empty list elements, each route
// with its own address.
func TestParseListSourceRoutes(t *testing.T) {
	runListCases(t, []listCase{
		{"imaptest fetch-envelope", "<@route:user@domain>", []imap.Address{routed("@route", "", "user", "domain")}},
		{"multiple hops", "<@first,@second:user@domain>", []imap.Address{routed("@first,@second", "", "user", "domain")}},
		{"display name", "Real Name <@route:user@domain>", []imap.Address{routed("@route", "Real Name", "user", "domain")}},
		{"encoded display name", "=?utf-8?q?Caf=C3=A9?= <@route:user@domain>", []imap.Address{routed("@route", "Café", "user", "domain")}},
		{"comments and mailbox CFWS", "Real Name < (before) @first (hop) , (between) @second : user (local) @ domain >",
			[]imap.Address{routed("@first,@second", "Real Name", "user", "domain")}},
		{"comments in hop domain", "<@first (c) . (c) example, @second:user@domain>", []imap.Address{routed("@first.example,@second", "", "user", "domain")}},
		{"folding white space", "<\r\n @first,\r\n\t@second:user@domain>", []imap.Address{routed("@first,@second", "", "user", "domain")}},
		{"empty route elements", "<, (before) , @first,, (between) @second, :user@domain>", []imap.Address{routed("@first,@second", "", "user", "domain")}},
		{"domain literals", "<@[2001:db8::1],@[192.0.2.1]:user@[2001:db8::2]>", []imap.Address{routed("@[2001:db8::1],@[192.0.2.1]", "", "user", "[2001:db8::2]")}},
		{"quoted pair in hop literal", `<@[x\]y]:user@domain>`, []imap.Address{routed(`@[x\]y]`, "", "user", "domain")}},
		{"quoted local part", `<@route:"x:y@z"@domain>`, []imap.Address{routed("@route", "", "x:y@z", "domain")}},
		{"quoted display name route text", `"Name: <@fake:other@domain>" <@route:user@domain>`,
			[]imap.Address{routed("@route", "Name: <@fake:other@domain>", "user", "domain")}},
		{"comment route text", "Name (not <@fake:other@domain>) <@route:user@domain>", []imap.Address{routed("@route", "Name", "user", "domain")}},
		{"mixed list", "plain@domain,, <@first:one@domain>, Two <two@domain>, <@second,@third:three@domain>", []imap.Address{
			addr("", "plain", "domain"), routed("@first", "", "one", "domain"), addr("Two", "two", "domain"), routed("@second,@third", "", "three", "domain"),
		}},
		{"group members and the addresses around the group", "<@a:before@domain>, team: one@domain, <@b:two@domain>;, <@c:after@domain>", []imap.Address{
			routed("@a", "", "before", "domain"), group("team"), addr("", "one", "domain"), routed("@b", "", "two", "domain"), groupEnd,
			routed("@c", "", "after", "domain"),
		}},
		{"a route does not end the group early", "Team: <@route (not > end):user@domain>;", []imap.Address{group("Team"), routed("@route", "", "user", "domain"), groupEnd}},
		{"a hop literal does not end the group early", "Team: <@[route>literal]:user@domain>;", []imap.Address{group("Team"), routed("@[route>literal]", "", "user", "domain"), groupEnd}},
		{"internationalized hop", "<@bücher.example:user@bücher.example>", []imap.Address{routed("@bücher.example", "", "user", "bücher.example")}},
	})
}

// Only a well-formed route is cut out; anything else is still not an address list.
func TestParseListMalformedSourceRoutes(t *testing.T) {
	for _, value := range []string{
		"<@:user@domain>",
		"<route:user@domain>",
		"<@first,second:user@domain>",
		"<@first @second:user@domain>",
		"<@first..example:user@domain>",
		"<@first.:user@domain>",
		"<@first words:user@domain>",
		"<@first;:user@domain>",
		"<@\"first\":user@domain>",
		"<@first:user@domain",
		"<@first:>",
		"<@first:not-an-address>",
		"<@first:other:user@domain>",
		"<@first (unfinished:user@domain>",
		"<@[unfinished:user@domain>",
		"<@[escaped\\]:user@domain>",
		"<@first\n:user@domain>",
		"<@first\r\n:user@domain>",
		"<@route\x80.example:user@domain>",
		"<(route\xff) @route:user@domain>",
	} {
		t.Run(value, func(t *testing.T) {
			if got, err := mailaddr.ParseList(value); err == nil {
				t.Errorf("ParseList(%q) = %+v, want an error", value, got)
			}
		})
	}
}

// What is not an address list is an error, so a caller can tell it from a list without
// addresses (an empty group).
func TestParseListErrors(t *testing.T) {
	for _, value := range []string{"", "   ", "not an address", "a@", "<>", ",,"} {
		if got, err := mailaddr.ParseList(value); err == nil {
			t.Errorf("ParseList(%q) = %+v, want an error", value, got)
		}
	}
	if got, err := mailaddr.ParseList("undisclosed-recipients:;"); err != nil || len(got) != 2 {
		t.Errorf("an empty group = %+v, %v; want its start and end", got, err)
	}
}

// A decoded name is text: an encoded-word that decodes to invalid UTF-8 or a NUL leaves
// neither in Name.
func TestParseListDecodedNamesAreText(t *testing.T) {
	runListCases(t, []listCase{
		{"invalid UTF-8 from an encoded-word", "=?utf-8?q?ab=80c?= <a@example.com>", []imap.Address{addr("abc", "a", "example.com")}},
		{"NUL from an encoded-word", "=?utf-8?q?ab=00c?= <a@example.com>", []imap.Address{addr("abc", "a", "example.com")}},
		{"in a group name", "=?utf-8?q?te=80am?=: a@example.com;", []imap.Address{group("team"), addr("", "a", "example.com"), groupEnd}},
	})
}

// FuzzParseList: no input panics; every entry is an address (mailbox and host), a group
// start (mailbox only) or a group end, with starts and ends paired and not nested; and a
// value net/mail parses gives the addresses net/mail gives, in order, with its display
// names when the value has no group.
func FuzzParseList(f *testing.F) {
	for _, seed := range []string{
		"a@example.com", "A <a@example.com>, b@example.com (B)", "group: g1@d1.org, g2@d2.org;, group2: g3@d3.org;",
		"undisclosed-recipients:;", `"Team: A" (x) :a@example.com;, =?utf-8?q?Caf=C3=A9?= list:;`, "<@first,@second:user@domain>",
		"Real Name < (before) @first (hop) , (between) @second : user (local) @ domain >", "<user (c)@ (c) example.com>",
		`"a@b"@example.com`, "team: a@[2001:db8::1];", "<@bücher.example:user@domain>", "a@b: x@y;", `"""":;`, "g: h: a@example.com;;",
		"=?utf-8?q?ab=80c?= <a@example.com>", "x@y, , ,", "<@[x\\]y]:user@domain>", "a@example.com; b@example.com",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		entries, err := mailaddr.ParseList(value)
		var flat []imap.Address
		inGroup := false
		for _, e := range entries {
			switch {
			case e.IsGroupStart():
				if inGroup || e.Name != "" || e.Route != "" {
					t.Fatalf("ParseList(%q): bad group start %+v in %+v", value, e, entries)
				}
				inGroup = true
			case e.IsGroupEnd():
				if !inGroup || e != (imap.Address{}) {
					t.Fatalf("ParseList(%q): bad group end %+v in %+v", value, e, entries)
				}
				inGroup = false
			default:
				if e.Mailbox == "" || e.Host == "" {
					t.Fatalf("ParseList(%q): entry %+v is neither an address nor a group marker", value, e)
				}
				flat = append(flat, e)
			}
		}
		if inGroup {
			t.Fatalf("ParseList(%q): unterminated group in %+v", value, entries)
		}
		want, mailErr := mail.ParseAddressList(value)
		if mailErr != nil {
			return
		}
		if err != nil {
			t.Fatalf("ParseList(%q): %v, but net/mail parses it to %v", value, err, want)
		}
		if len(flat) != len(want) {
			t.Fatalf("ParseList(%q) has %d addresses %+v, net/mail %d %v", value, len(flat), flat, len(want), want)
		}
		for i, a := range want {
			if got := flat[i].Mailbox + "@" + flat[i].Host; got != a.Address || flat[i].Route != "" {
				t.Fatalf("ParseList(%q): address %d is %+v, net/mail has %q", value, i, flat[i], a.Address)
			}
			// A group member is parsed on its own, where net/mail takes a trailing comment
			// for the display name, which it does not inside a group.
			if wantName := strings.ReplaceAll(strings.ToValidUTF8(a.Name, ""), "\x00", ""); len(flat) == len(entries) && flat[i].Name != wantName {
				t.Fatalf("ParseList(%q): address %d has name %q, net/mail %q", value, i, flat[i].Name, a.Name)
			}
		}
	})
}
