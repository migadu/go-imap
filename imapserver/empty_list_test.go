package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// Where the grammar writes a list as "(" x *(SP x) ")", the list holds at least
// one element, and an empty "()" is invalid arguments: "BAD - command unknown
// or arguments invalid" (RFC 9051 §6.4.5 for FETCH, §6.3.11 for STATUS).
//
//	fetch           = "FETCH" SP sequence-set SP (
//	                  "ALL" / "FULL" / "FAST" /
//	                  fetch-att / "(" fetch-att *(SP fetch-att) ")")
//	header-list     = "(" header-fld-name *(SP header-fld-name) ")"
//	status          = "STATUS" SP mailbox SP
//	                  "(" status-att *(SP status-att) ")"
//	search-key      = ... / "(" search-key *(SP search-key) ")"
//
// (RFC 9051 §9), sort-criteria = "(" sort-criterion *(SP sort-criterion) ")"
// (RFC 5256 §5), status-option = "STATUS" SP "(" status-att *(SP status-att)
// ")" (RFC 5819 §4) and return-option =/ "METADATA" SP "(" entry *(SP entry)
// ")" (RFC 9590 §5). Each was answered OK, "FETCH 1 ()" with "* 1 FETCH ()",
// which msg-att does not allow, and "FETCH 1 BODY.PEEK[HEADER.FIELDS ()]" with
// the whole header as BODY[HEADER].
func TestEmptyRequiredListIsBAD(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1:    {},
		imap.CapSort:         {},
		imap.CapESort:        {},
		imap.CapESearch:      {},
		imap.CapListExtended: {},
		imap.CapListStatus:   {},
		imap.CapListMetadata: {},
	})
	rc.appendOne("empty lists")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		`FETCH 1 ()`,
		`UID FETCH 1 ()`,
		`FETCH 1 BODY.PEEK[HEADER.FIELDS ()]`,
		`FETCH 1 BODY[HEADER.FIELDS.NOT ()]`,
		`FETCH 1 (FLAGS BODY.PEEK[HEADER.FIELDS ()])`,
		`STATUS INBOX ()`,
		`SORT () UTF-8 ALL`,
		`SORT (DATE) UTF-8 ()`,
		`SEARCH ()`,
		`SEARCH NOT ()`,
		`SEARCH OR () ALL`,
		`SEARCH OR ALL ()`,
		`SEARCH (ALL ())`,
		`SEARCH CHARSET UTF-8 ()`,
		`UID SEARCH ()`,
		`THREAD REFERENCES UTF-8 ()`,
		`LIST "" "*" RETURN (STATUS ())`,
		`LIST "" "*" RETURN (METADATA ())`,
	} {
		resp, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
			t.Errorf("%s → %s, want BAD [CLIENTBUG]", cmd, strings.TrimRight(resp, "\r\n"))
		} else if resp != tagged {
			t.Errorf("%s: want no untagged response, got:\n%s", cmd, resp)
		}
		rc.requireInSync(cmd)
	}

	// The same lists with an element are served, and the lists the grammar
	// lets be empty stay accepted: flag-list "(" [flag *(SP flag)] ")" (RFC
	// 9051 §9), list-select-opts and list-return-opts (RFC 5258 §4),
	// search-return-opts (RFC 4731 §3.1) and sort-return-opts (RFC 5267 §3).
	for _, tc := range []struct{ cmd, want string }{
		{`FETCH 1 (FLAGS)`, " FLAGS ("},
		{`FETCH 1 BODY.PEEK[HEADER.FIELDS (Subject)]`, `BODY[HEADER.FIELDS ("Subject")] {24}`},
		{`STATUS INBOX (MESSAGES)`, "* STATUS INBOX (MESSAGES 1)"},
		{`SORT (DATE) UTF-8 ALL`, "* SORT 1"},
		{`SEARCH (ALL)`, "* SEARCH 1"},
		{`SEARCH NOT (DELETED)`, "* SEARCH 1"},
		{`THREAD REFERENCES UTF-8 (ALL)`, "* THREAD (1)"},
		{`LIST "" "*" RETURN (STATUS (MESSAGES))`, "* STATUS INBOX (MESSAGES 1)"},
		{`STORE 1 FLAGS (\Flagged)`, " FLAGS ("},
		{`STORE 1 FLAGS ()`, " FLAGS ())"},
		{`LIST () "" "*"`, `* LIST () "/" INBOX`},
		{`LIST "" "*" RETURN ()`, `* LIST () "/" INBOX`},
		{`SEARCH RETURN () ALL`, " ALL 1"},
		{`SORT RETURN () (DATE) UTF-8 ALL`, " ALL 1"},
	} {
		if resp := rc.do(tc.cmd); !strings.Contains(resp, tc.want) {
			t.Errorf("%s: want %q, got:\n%s", tc.cmd, tc.want, resp)
		}
	}
	if tagged, _ := rc.exchange("APPEND INBOX () {5}", "hello"); !strings.Contains(tagged, " OK ") {
		t.Errorf("APPEND INBOX () → %s, want OK", strings.TrimRight(tagged, "\r\n"))
	}
}
