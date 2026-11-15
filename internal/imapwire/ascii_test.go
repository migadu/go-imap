package imapwire

import "testing"

func TestASCIICaseFolding(t *testing.T) {
	const (
		longS    = "ſ" // strings.ToUpper: "S"
		dotlessI = "ı" // strings.ToUpper: "I"
		dottedI  = "İ" // strings.ToLower: "i"
		kelvin   = "K" // strings.ToLower: "k"
	)
	for _, tc := range []struct{ in, upper, lower string }{
		{"", "", ""},
		{"UID", "UID", "uid"},
		{"Search", "SEARCH", "search"},
		{"rfc822.size", "RFC822.SIZE", "rfc822.size"},
		{"[]-_09", "[]-_09", "[]-_09"},
		{longS + "elect", longS + "ELECT", longS + "elect"},
		{"u" + dotlessI + "d", "U" + dotlessI + "D", "u" + dotlessI + "d"},
		{dottedI + "NBOX", dottedI + "NBOX", dottedI + "nbox"},
		{kelvin + "EYWORD", kelvin + "EYWORD", kelvin + "eyword"},
		{"\xff\x80z", "\xff\x80Z", "\xff\x80z"},
	} {
		if got := ToUpperASCII(tc.in); got != tc.upper {
			t.Errorf("ToUpperASCII(%q) = %q, want %q", tc.in, got, tc.upper)
		}
		if got := ToLowerASCII(tc.in); got != tc.lower {
			t.Errorf("ToLowerASCII(%q) = %q, want %q", tc.in, got, tc.lower)
		}
	}

	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"NIL", "nil", true},
		{"Charset", "CHARSET", true},
		{"CHAR" + longS + "ET", "CHARSET", false},
		{"S" + kelvin + "IP", "SKIP", false},
		{"NIL", "NI", false},
		{"[", "{", false}, // 0x5B and 0x7B differ by the ASCII case bit
		{"@", "`", false},
	} {
		if got := EqualFoldASCII(tc.a, tc.b); got != tc.want {
			t.Errorf("EqualFoldASCII(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
