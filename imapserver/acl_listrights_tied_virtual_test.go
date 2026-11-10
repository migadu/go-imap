package imapserver

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// RFC 4314 §2.1.1 requires a virtual right and all its members to share
// the non-member rights to which any member is tied. The writer may add a
// virtual letter to an existing group, but cannot invent ties between the
// real rights supplied by the session.
func TestListRightsTiedVirtualRights(t *testing.T) {
	runListRightsCases(t, func() Session { return &aclRecordingSession{} }, []listRightsCase{
		{
			name:     "create_tied_to_seen",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("skx"), imap.RightSet("t"), imap.RightSet("e")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "skxc" "t" "e" "d"`,
		},
		{
			name:     "both_families_tied_together",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("kxte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "kxtecd"`,
		},
		{
			name:     "explicit_singleton_is_moved_into_tied_group",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("d"), imap.RightSet("swte"), imap.RightSet("k"), imap.RightSet("x")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "swted" "k" "x" "c"`,
		},
		// `c` is tied to `s` with its members; `t` and `e` are tied to no
		// non-member, so `d` is returned by itself.
		{
			name:     "explicit_virtuals_are_placed_from_their_members",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("skxc"), imap.RightSet("ted")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "skxc" "te" "d"`,
		},
		{
			name:     "both_explicit_virtuals_in_shared_group",
			optional: []imap.RightSet{imap.RightSet("kxtecd")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "kxtecd"`,
		},
		{
			name:     "explicit_required_virtual_follows_its_optional_members",
			required: imap.RightSet("d"),
			optional: []imap.RightSet{imap.RightSet("swte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "swted"`,
		},
		{
			name:     "explicit_virtual_in_another_group_follows_its_members",
			optional: []imap.RightSet{imap.RightSet("sd"), imap.RightSet("wte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "s" "wted"`,
		},
		// No create member is on offer, so the session's `c` is not a right
		// the identifier can be granted and does not tie `t` to anything.
		{
			name:     "explicit_virtual_without_members_is_dropped",
			optional: []imap.RightSet{imap.RightSet("tc"), imap.RightSet("d")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "t" "d"`,
		},
	})
}

// A member that is neither required nor optional cannot be granted on this
// mailbox, and one that is required is granted whatever the client asks. Only
// the optional members can be tied, so they alone decide the group: RFC 4314
// §2.1.1's first example advertises `d` for a server that does not offer `e`.
func TestListRightsTiedVirtualRightsCountOfferedMembersOnly(t *testing.T) {
	runListRightsCases(t, func() Session { return &aclRecordingSession{} }, []listRightsCase{
		{
			name:     "delete_member_not_offered",
			optional: []imap.RightSet{imap.RightSet("swt")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "swtd"`,
		},
		{
			name:     "create_member_not_offered",
			optional: []imap.RightSet{imap.RightSet("sk")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "skc"`,
		},
		// A mailbox that cannot be deleted: everything else in one group.
		{
			name:     "one_group_without_x",
			optional: []imap.RightSet{imap.RightSet("lrswipktea")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lrswipkteacd"`,
		},
		{
			name:     "one_member_of_each_family_not_offered",
			optional: []imap.RightSet{imap.RightSet("skxt")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "skxtcd"`,
		},
		// `x` is a create member by default: it ties `c` to this group too.
		{
			name:     "x_tied_to_delete_members",
			optional: []imap.RightSet{imap.RightSet("sxte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "sxtecd"`,
		},
		{
			name:     "one_member_required_other_tied",
			required: imap.RightSet("t"),
			optional: []imap.RightSet{imap.RightSet("swe")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "t" "swed"`,
		},
	})
}

// The four LISTRIGHTS examples of RFC 4314 §2.1.1, each under the family the
// RFC states for it. Only the order differs from the RFC's text: a letter
// returned by itself is written after the session's groups.
func TestListRightsRFC4314Examples(t *testing.T) {
	groups := func(s string) []imap.RightSet {
		var sets []imap.RightSet
		for _, g := range strings.Fields(s) {
			sets = append(sets, imap.RightSet(g))
		}
		return sets
	}
	// "and c=k": the second family, `d` = `e`+`t`+`x`.
	secondFamily := func() Session {
		return &aclFamilySession{create: imap.RightSet("k"), delete: imap.RightSet("etx")}
	}
	runListRightsCases(t, secondFamily, []listRightsCase{
		{
			name:     "lr_s_w_i_p_k_x_t",
			optional: groups("lr s w i p k x t"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lr" "s" "w" "i" "p" "k" "x" "t" "c" "d"`,
		},
		{
			name:     "lr_s_w_i_p_k_xte",
			optional: groups("lr s w i p k xte"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lr" "s" "w" "i" "p" "k" "xte" "c" "d"`,
		},
		{
			name:     "lr_s_w_i_p_k_x_te",
			optional: groups("lr s w i p k x te"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lr" "s" "w" "i" "p" "k" "x" "te" "c" "d"`,
		},
	})
	// "and c=kx": the first family, which is the default.
	runListRightsCases(t, func() Session { return &aclRecordingSession{} }, []listRightsCase{
		{
			name:     "lr_swte_i_p_k_x",
			optional: groups("lr swte i p k x"),
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lr" "swted" "i" "p" "k" "x" "c"`,
		},
	})
}

func TestListRightsTiedVirtualRightsFollowDeclaration(t *testing.T) {
	secondFamily := func() Session {
		return &aclFamilySession{create: imap.RightSet("k"), delete: imap.RightSet("xte")}
	}
	runListRightsCases(t, secondFamily, []listRightsCase{
		{
			name:     "all_delete_members_without_nonmember",
			optional: []imap.RightSet{imap.RightSet("xte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "xte" "d"`,
		},
		{
			name:     "all_delete_members_tied_to_seen",
			optional: []imap.RightSet{imap.RightSet("sxte")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "sxted"`,
		},
		{
			name:     "create_requires_only_k",
			optional: []imap.RightSet{imap.RightSet("sk")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "skc"`,
		},
	})
	runListRightsCases(t, func() Session { return &aclFamilySession{} }, []listRightsCase{
		{
			name:     "empty_families_are_not_advertised",
			optional: []imap.RightSet{imap.RightSet("st")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "st"`,
		},
	})
}

// §2.1.1: when any member is tied to a non-member, the virtual right "and all
// of the member rights need to be tied to the same non-member right(s)". A
// session whose optional members sit in different groups breaks that, and the
// writer cannot mend it without inventing a tie, so nothing is written.
func TestListRightsTiedVirtualRightsRejectSplitMembers(t *testing.T) {
	cases := []struct {
		name     string
		optional []string
		right    imap.Right // the virtual right whose members are split
	}{
		{"delete_member_tied_other_alone", []string{"swt", "e"}, imap.RightDelete},
		{"delete_members_tied_to_different_nonmembers", []string{"st", "we"}, imap.RightDelete},
		{"create_member_tied_other_alone", []string{"sk", "x"}, imap.RightCreate},
		{"explicit_virtual_does_not_hide_split_members", []string{"sdt", "e"}, imap.RightDelete},
		// Both virtual letters must validate before the encoder writes a line:
		// c fits the first group, but d's members are split.
		{"delete_split_after_create_fits", []string{"skxt", "e"}, imap.RightDelete},
		// The RFC's second example is a c=k server. Under the default family
		// `x` is a create member, tied here to `t` and `e` while `k` is not:
		// such a backend has to declare its family (SessionACLVirtualRights).
		{"second_family_layout_undeclared", []string{"lr", "s", "w", "i", "p", "k", "xte"}, imap.RightCreate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := &imap.ListRightsData{Mailbox: "INBOX", Identifier: "owner@example.com"}
			for _, group := range tc.optional {
				data.OptionalRights = append(data.OptionalRights, imap.RightSet(group))
			}
			var out bytes.Buffer
			conn := newACLTestConn(t, &aclRecordingSession{}, &out)
			err := conn.writeListRights(data)
			if err == nil {
				t.Error("writeListRights succeeded although the members are split")
			} else if want := "virtual " + string(tc.right); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
			if err := conn.bw.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if out.Len() != 0 {
				t.Errorf("wrote partial response before rejecting declaration: %q", out.String())
			}
		})
	}
}

func TestListRightsTiedVirtualRightsDoNotMutateSessionData(t *testing.T) {
	// Spare capacity makes an accidental append into a session-owned slice
	// observable even when its original length remains unchanged.
	withSpare := func(s string) imap.RightSet {
		rs := make(imap.RightSet, len(s)+4)
		copy(rs, imap.RightSet(s))
		copy(rs[len(s):], imap.RightSet("9876"))
		return rs[:len(s)]
	}
	data := &imap.ListRightsData{
		Mailbox: "INBOX", Identifier: "owner@example.com", RequiredRights: withSpare("lr"),
		OptionalRights: []imap.RightSet{withSpare("d"), withSpare("swte"), withSpare("k"), withSpare("x")},
	}
	backing := func(data *imap.ListRightsData) []imap.RightSet {
		sets := append([]imap.RightSet{data.RequiredRights}, data.OptionalRights...)
		var snapshot []imap.RightSet
		for _, set := range sets {
			snapshot = append(snapshot, append(imap.RightSet(nil), set[:cap(set)]...))
		}
		return snapshot
	}
	before := backing(data)
	for i := 0; i < 2; i++ {
		line := renderListRights(t, &aclRecordingSession{}, data)
		want := `* LISTRIGHTS INBOX "owner@example.com" "lr" "swted" "k" "x" "c"`
		if line != want {
			t.Errorf("serialization %d: got %s; want %s", i+1, line, want)
		}
		checkListRightsOnce(t, line)
		if after := backing(data); !reflect.DeepEqual(after, before) {
			t.Errorf("serialization %d mutated session-owned backing arrays: got %q; want %q", i+1, after, before)
		}
	}
}
