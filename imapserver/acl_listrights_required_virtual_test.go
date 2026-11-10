package imapserver

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// listRightsCase is one LISTRIGHTS answer a session hands writeListRights and
// the exact line the client must receive for it.
type listRightsCase struct {
	name     string
	required imap.RightSet
	optional []imap.RightSet
	want     string
}

// renderListRights writes data for a session and returns the response line
// without its CRLF.
func renderListRights(t *testing.T, session Session, data *imap.ListRightsData) string {
	t.Helper()
	var out bytes.Buffer
	conn := newACLTestConn(t, session, &out)
	if err := conn.writeListRights(data); err != nil {
		t.Fatalf("writeListRights: %v", err)
	}
	if err := conn.bw.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return strings.TrimRight(out.String(), "\r\n")
}

var quotedString = regexp.MustCompile(`"([^"]*)"`)

// checkListRightsOnce enforces RFC 4314 §3.7, "The same right MUST NOT be
// listed more than once": across the required string and every optional
// group, each right may appear at most once.
func checkListRightsOnce(t *testing.T, line string) {
	t.Helper()
	strs := quotedString.FindAllStringSubmatch(line, -1)
	if len(strs) < 2 {
		t.Fatalf("no identifier and required rights in %q", line)
	}
	seen := make(map[rune]bool)
	for _, s := range strs[1:] { // strs[0] is the identifier
		for _, r := range s[1] {
			if seen[r] {
				t.Errorf("%s: right %q listed more than once", line, r)
			}
			seen[r] = true
		}
	}
}

func runListRightsCases(t *testing.T, newSession func() Session, cases []listRightsCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := renderListRights(t, newSession(), &imap.ListRightsData{
				Mailbox:        "INBOX",
				Identifier:     "owner@example.com",
				RequiredRights: tc.required,
				OptionalRights: tc.optional,
			})
			if line != tc.want {
				t.Errorf("required %q, optional %q:\n got  %s\n want %s", tc.required, tc.optional, line, tc.want)
			}
			checkListRightsOnce(t, line)
		})
	}
}

// TestListRightsVirtualRightOfRequiredMembersIsRequired pins where
// writeListRights puts the virtual `c` and `d` (RFC 4314 §2.1.1) under the
// default family, `c` = `k`+`x` and `d` = `t`+`e`.
//
// RFC 4314 §3.7 makes the first rights string the set "the identifier will
// always be granted in the mailbox" and each following string a set it "can
// be granted". When every member of a virtual right is required, the virtual
// right is always granted too, so it belongs in the required string. Written
// as its own optional group it tells an RFC 2086 client that the right can be
// granted, which the required string contradicts. Before the fix an owner for
// whom every right is required was answered
//
//	S: * LISTRIGHTS INBOX "owner@example.com" "lrswipkxtea" "c" "d"
//
// When the members are grantable only, or are split between required and
// optional, and none is tied to a non-member, the virtual right is a group of
// its own. TestListRightsTiedVirtualRights covers the tied layouts.
func TestListRightsVirtualRightOfRequiredMembersIsRequired(t *testing.T) {
	runListRightsCases(t, func() Session { return &aclRecordingSession{} }, []listRightsCase{
		// (a) Every member required: the letter joins the required string.
		{
			name:     "owner_all_required",
			required: imap.RightSet("lrswipkxtea"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxteacd"`,
		},
		{
			name:     "create_members_required_delete_members_optional",
			required: imap.RightSet("lrkx"),
			optional: []imap.RightSet{imap.RightSet("s"), imap.RightSet("t"), imap.RightSet("e")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrkxc" "s" "t" "e" "d"`,
		},
		{
			name:     "delete_members_required_create_members_optional",
			required: imap.RightSet("lrte"),
			optional: []imap.RightSet{imap.RightSet("k"), imap.RightSet("x")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrted" "k" "x" "c"`,
		},
		// (b) Members only optional: a group of its own, unless a member is
		// tied to a non-member, when the letter joins that group.
		{
			name:     "members_only_optional",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("k"), imap.RightSet("x"), imap.RightSet("t"), imap.RightSet("e")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "k" "x" "t" "e" "c" "d"`,
		},
		{
			name:     "empty_required",
			required: imap.RightSet(""),
			optional: []imap.RightSet{imap.RightSet("lr"), imap.RightSet("kxte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lr" "kxtecd"`,
		},
		// (c) Members split between required and optional: still a group of
		// its own, since granting it grants the optional member.
		{
			name:     "members_split",
			required: imap.RightSet("lrkt"),
			optional: []imap.RightSet{imap.RightSet("x"), imap.RightSet("e")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrkt" "x" "e" "c" "d"`,
		},
		{
			name:     "create_split_delete_required",
			required: imap.RightSet("lrswikte"),
			optional: []imap.RightSet{imap.RightSet("x")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswikted" "x" "c"`,
		},
		// No member anywhere: no virtual right.
		{
			name:     "no_members",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("s")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "s"`,
		},
		// (d) A member that is neither required nor optional cannot be
		// granted and does not count: the members on offer decide.
		{
			name:     "one_member_required_other_not_offered",
			required: imap.RightSet("lrk"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrkc"`,
		},
		{
			name:     "one_member_optional_other_not_offered",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("t")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "t" "d"`,
		},
		// (e) A virtual letter the session lists is not taken over: the
		// letter is placed from its members, and never written twice.
		{
			name:     "caller_listed_both_in_required",
			required: imap.RightSet("lrswipkxteacd"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxteacd"`,
		},
		{
			name:     "caller_listed_c_as_optional",
			required: imap.RightSet("lrkx"),
			optional: []imap.RightSet{imap.RightSet("c")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrkxc"`,
		},
		{
			name:     "caller_listed_d_as_optional_c_derived",
			required: imap.RightSet("lrswipkxtea"),
			optional: []imap.RightSet{imap.RightSet("d")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxteacd"`,
		},
		{
			name:     "caller_listed_d_as_required_members_optional",
			required: imap.RightSet("lrd"),
			optional: []imap.RightSet{imap.RightSet("t"), imap.RightSet("e")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "t" "e" "d"`,
		},
		// §2.1.1: members not tied to a non-member, so the letter "is
		// returned by itself".
		{
			name:     "caller_tied_d_to_its_members",
			required: imap.RightSet("lrkx"),
			optional: []imap.RightSet{imap.RightSet("ted")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lrkxc" "te" "d"`,
		},
		{
			name:     "caller_tied_c_to_one_member",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("kc"), imap.RightSet("x")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "k" "x" "c"`,
		},
	})
}

// TestListRightsVirtualRightOfRequiredMembersFollowsDeclaration runs the same
// rule against declarations other than the default (SessionACLVirtualRights):
// "every member is required" is judged against the members the session
// declares, and a virtual right declared without members is never written,
// however many rights are required.
func TestListRightsVirtualRightOfRequiredMembersFollowsDeclaration(t *testing.T) {
	declared := func(create, delete string) func() Session {
		return func() Session {
			return &aclFamilySession{create: imap.RightSet(create), delete: imap.RightSet(delete)}
		}
	}

	t.Run("c=k,d=te", func(t *testing.T) {
		runListRightsCases(t, declared("k", "te"), []listRightsCase{
			{
				name:     "owner_all_required",
				required: imap.RightSet("lrswipkxtea"),
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxteacd"`,
			},
			// `x` is a member of neither, so `k` alone makes `c` required.
			{
				name:     "k_required_x_optional",
				required: imap.RightSet("lrk"),
				optional: []imap.RightSet{imap.RightSet("x")},
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrkc" "x"`,
			},
			{
				name:     "x_required_only",
				required: imap.RightSet("lrx"),
				optional: []imap.RightSet{imap.RightSet("k")},
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrx" "k" "c"`,
			},
		})
	})

	t.Run("c=k,d=xte", func(t *testing.T) {
		runListRightsCases(t, declared("k", "xte"), []listRightsCase{
			{
				name:     "owner_all_required",
				required: imap.RightSet("lrswipkxtea"),
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxteacd"`,
			},
			// `x` belongs to `d` here: with `x` optional, `d` is split.
			{
				name:     "x_optional_splits_d",
				required: imap.RightSet("lrswikte"),
				optional: []imap.RightSet{imap.RightSet("x")},
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswiktec" "x" "d"`,
			},
		})
	})

	t.Run("c=kx,no_d", func(t *testing.T) {
		runListRightsCases(t, declared("kx", ""), []listRightsCase{
			{
				name:     "owner_all_required",
				required: imap.RightSet("lrswipkxtea"),
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxteac"`,
			},
		})
	})

	t.Run("no_c,no_d", func(t *testing.T) {
		runListRightsCases(t, declared("", ""), []listRightsCase{
			{
				name:     "owner_all_required",
				required: imap.RightSet("lrswipkxtea"),
				want:     `* LISTRIGHTS INBOX "owner@example.com" "lrswipkxtea"`,
			},
			{
				name:     "empty_required",
				required: imap.RightSet(""),
				optional: []imap.RightSet{imap.RightSet("kxte")},
				want:     `* LISTRIGHTS INBOX "owner@example.com" "" "kxte"`,
			},
		})
	})
}
