package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// Non-ASCII letters that Unicode case mapping turns into the ASCII letters of
// a keyword: strings.ToUpper maps U+017F to "S" and U+0131 to "I", and
// strings.EqualFold matches U+017F with "s" and U+212A with "k".
const (
	longS    = "ſ" // LATIN SMALL LETTER LONG S
	dotlessI = "ı" // LATIN SMALL LETTER DOTLESS I
	kelvin   = "K" // KELVIN SIGN
)

var asciiKeywordCaps = imap.CapSet{
	imap.CapIMAP4rev1:    {},
	imap.CapEnable:       {},
	imap.CapCondStore:    {},
	imap.CapESearch:      {},
	imap.CapSort:         {},
	imap.CapESort:        {},
	imap.CapListExtended: {},
}

// "ABNF strings are case insensitive and the character set for these strings
// is US-ASCII" (RFC 5234 §2.3), and RFC 9051 §9 defines its keywords in ABNF.
// A token with a non-ASCII letter in it is none of them, however it maps under
// Unicode case rules.
func TestKeywordLookalikesBeforeAuthAreUnknown(t *testing.T) {
	for _, cmd := range []string{
		"CAPAB" + dotlessI + "LITY",
		"LOG" + dotlessI + "N " + searchRev2Username + " " + searchRev2Password,
		"AUTHENTICATE PLA" + dotlessI + "N",
	} {
		// Read a single line: a mechanism taken for PLAIN answers with a
		// continuation request rather than a tagged response.
		rc := dialSearchRev2Conn(t, asciiKeywordCaps, nil)
		if _, err := rc.c.Write([]byte("t1 " + cmd + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", cmd, err)
		}
		line, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response to %q: %v", cmd, err)
		}
		if !strings.HasPrefix(line, "t1 BAD ") && !strings.HasPrefix(line, "t1 NO ") {
			t.Errorf("%s → %s, want it refused", cmd, strings.TrimRight(line, "\r\n"))
		}
	}

	// Case-insensitive ASCII is still the command.
	rc := dialSearchRev2Conn(t, asciiKeywordCaps, nil)
	if resp := rc.do("capability"); !strings.Contains(resp, "* CAPABILITY ") {
		t.Errorf("capability: want a CAPABILITY response, got:\n%s", resp)
	}
}

func TestKeywordLookalikesAreNotKeywords(t *testing.T) {
	rc := newSearchRev2Conn(t, asciiKeywordCaps)
	rc.appendOne("sort")
	rc.do("SELECT INBOX")

	const refused = "" // BAD or NO, with no untagged response
	for _, tc := range []struct{ cmd, want string }{
		// Command names and the UID sub-command (Conn.readCommand).
		{longS + "ELECT INBOX", " BAD "},
		{longS + "TATUS INBOX (MESSAGES)", " BAD "},
		{"U" + dotlessI + "D SEARCH ALL", " BAD "},
		{"UID " + longS + "EARCH ALL", " BAD "},
		{"UID " + longS + "TORE 1 +FLAGS (\\Seen)", " BAD "},
		{"CAPAB" + dotlessI + "LITY", " BAD "},

		// STATUS items.
		{"STATUS INBOX (MESSAGE" + longS + ")", " BAD "},
		{"STATUS INBOX (U" + dotlessI + "DNEXT)", " BAD "},

		// FETCH items, macros and section specifiers; STORE items.
		{"FETCH 1 (FLAG" + longS + ")", " BAD "},
		{"FETCH 1 " + dotlessI + "NTERNALDATE", " BAD "},
		{"FETCH 1 RFC822." + longS + "IZE", " BAD "},
		{"FETCH 1 FA" + longS + "T", " BAD "},
		{"FETCH 1 BODY.PEEK[HEADER.F" + dotlessI + "ELDS (Subject)]", " BAD "},
		{"STORE 1 +FLAG" + longS + " (\\Seen)", " BAD "},

		// Search keys, CHARSET and RETURN options.
		{"SEARCH " + longS + "UBJECT sort", " BAD "},
		{"SEARCH UN" + longS + "EEN", " BAD "},
		{"SEARCH NOT " + longS + "EEN", " BAD "},
		{"SEARCH " + kelvin + "EYWORD foo", " BAD "},
		{"SEARCH CHAR" + longS + "ET UTF-8 ALL", " BAD "},
		{"SEARCH RETURN (M" + dotlessI + "N) ALL", " BAD "},
		{"SEARCH CHARSET U" + longS + "-A" + longS + "C" + dotlessI + "I ALL", " NO [BADCHARSET] "},

		// SORT keys, REVERSE, RETURN options and charset.
		{"SORT (" + longS + "IZE) UTF-8 ALL", refused},
		{"SORT (REVER" + longS + "E DATE) UTF-8 ALL", refused},
		{"SORT RETURN (M" + dotlessI + "N) (DATE) UTF-8 ALL", " BAD "},
		{"SORT (DATE) U" + longS + "-A" + longS + "C" + dotlessI + "I ALL", " NO [BADCHARSET] "},

		// LIST selection and return options.
		{"LIST (" + longS + "UBSCRIBED) \"\" \"*\"", " BAD "},
		{"LIST \"\" \"*\" RETURN (" + longS + "UBSCRIBED)", " BAD "},

		// Last: on success it would leave the mailbox read-only.
		{"EXAM" + dotlessI + "NE INBOX", " BAD "},
	} {
		resp, tagged := rc.try(tc.cmd)
		switch {
		case tc.want == refused && strings.Contains(tagged, " OK "),
			tc.want != refused && !strings.Contains(tagged, tc.want):
			want := tc.want
			if want == refused {
				want = "BAD or NO"
			}
			t.Errorf("%s → %s, want %q", tc.cmd, strings.TrimRight(tagged, "\r\n"), want)
		case resp != tagged:
			t.Errorf("%s: want no untagged response, got:\n%s", tc.cmd, resp)
		}
		rc.requireInSync(tc.cmd)
	}

	// The same keywords in ASCII, in any case, are served.
	for _, tc := range []struct{ cmd, want string }{
		{"status INBOX (messages uidnext)", "* STATUS INBOX (MESSAGES 1 UIDNEXT "},
		{"uid search all", "* SEARCH 1"},
		{"fetch 1 (flags rfc822.size internaldate)", "* 1 FETCH ("},
		{"fetch 1 body.peek[header.fields (Subject)]", `BODY[HEADER.FIELDS ("Subject")]`},
		{"search charset us-ascii subject sort", "* SEARCH 1"},
		{"search return (min) all", " MIN 1"},
		{"sort (reverse size) us-ascii all", "* SORT 1"},
		{"list (subscribed) \"\" \"*\"", " OK "},
	} {
		if resp := rc.do(tc.cmd); !strings.Contains(resp, tc.want) {
			t.Errorf("%s: want %q, got:\n%s", tc.cmd, tc.want, resp)
		}
	}
}

// ENABLE names capabilities, which are atoms compared without regard to ASCII
// case (RFC 5161 §3.1, RFC 9051 §9). One spelled with a look-alike names no
// capability the server knows, so it is not enabled.
func TestEnableLookalikeCapabilityIsNotEnabled(t *testing.T) {
	rc := newSearchRev2Conn(t, asciiKeywordCaps)
	if resp := rc.do("ENABLE COND" + longS + "TORE"); strings.Contains(resp, "CONDSTORE") {
		t.Errorf("ENABLE COND%sTORE enabled CONDSTORE:\n%s", longS, resp)
	}
	if resp := rc.do("enable condstore"); !strings.Contains(resp, "* ENABLED CONDSTORE") {
		t.Errorf("enable condstore: want CONDSTORE enabled, got:\n%s", resp)
	}
}

// HEADER.FIELDS lists header field names (RFC 9051 §6.4.5), which are
// printable US-ASCII (RFC 5322 §2.2) compared without regard to ASCII case.
// strings.ToLower made a field named with U+212A or U+0130 match one spelled
// with "k" or "i"; HEADER.FIELDS.NOT already removed ASCII case variants only.
func TestExtractBodySectionHeaderFieldsLookalikes(t *testing.T) {
	const msg = "Keywords: a\r\nMIME-Version: 1.0\r\nSubject: x\r\n\r\nbody\r\n"
	for _, tc := range []struct {
		fields []string
		want   string
	}{
		{[]string{kelvin + "eywords", "MİME-Version"}, "\r\n"},
		{[]string{"KEYWORDS", "mime-version"}, "Keywords: a\r\nMIME-Version: 1.0\r\n\r\n"},
	} {
		got := imapserver.ExtractBodySection(strings.NewReader(msg), &imap.FetchItemBodySection{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: tc.fields,
		})
		if string(got) != tc.want {
			t.Errorf("HEADER.FIELDS %q = %q, want %q", tc.fields, got, tc.want)
		}
	}
}
