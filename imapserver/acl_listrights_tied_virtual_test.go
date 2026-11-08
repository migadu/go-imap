package imapserver

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// RFC 4314 §2.1.1 requires a virtual right and all its members to share
// the non-member rights to which any member is tied. The writer may add a
// virtual letter to an existing group, but cannot invent ties between the
// real rights supplied by the session.
func TestListRightsTiedVirtualRights(t *testing.T) {
	runListRightsCases(t, func() Session { return &aclRecordingSession{} }, []listRightsCase{
		{
			name:     "rfc_swte_example",
			optional: []imap.RightSet{imap.RightSet("lr"), imap.RightSet("swte"), imap.RightSet("i"), imap.RightSet("p"), imap.RightSet("k"), imap.RightSet("x")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "lr" "swted" "i" "p" "k" "x" "c"`,
		},
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
		{
			name:     "explicit_virtual_already_in_correct_group",
			required: imap.RightSet("lr"),
			optional: []imap.RightSet{imap.RightSet("skxc"), imap.RightSet("ted")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "lr" "skxc" "ted"`,
		},
		{
			name:     "both_explicit_virtuals_in_shared_group",
			optional: []imap.RightSet{imap.RightSet("kxtecd")},
			want:     `* LISTRIGHTS INBOX "owner@example.com" "" "kxtecd"`,
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

func TestListRightsTiedVirtualRightsRejectInconsistentGroups(t *testing.T) {
	cases := []struct {
		name     string
		required string
		optional []string
	}{
		{"members_in_separate_optional_groups", "", []string{"swt", "e"}},
		{"members_split_required_and_optional", "t", []string{"swe"}},
		{"member_not_grantable", "", []string{"swt"}},
		{"members_tied_to_different_nonmembers", "", []string{"st", "we"}},
		{"default_create_needs_x", "", []string{"sk"}},
		{"default_x_is_not_a_delete_member", "", []string{"sxte"}},
		{"explicit_virtual_is_required", "d", []string{"swte"}},
		{"explicit_virtual_in_another_tied_group", "", []string{"sd", "wte"}},
		{"explicit_virtual_does_not_hide_missing_members", "", []string{"sdt", "e"}},
		// Both virtual letters must validate before the encoder writes a line:
		// c fits the group below, but d is missing its e member.
		{"second_family_error_after_first_family_fits", "", []string{"skxt"}},
		// An obsolete letter from the other family is still a non-member.
		{"opposite_virtual_cannot_replace_missing_member", "", []string{"tc", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := &imap.ListRightsData{
				Mailbox: "INBOX", Identifier: "owner@example.com", RequiredRights: imap.RightSet(tc.required),
			}
			for _, group := range tc.optional {
				data.OptionalRights = append(data.OptionalRights, imap.RightSet(group))
			}
			var out bytes.Buffer
			conn := newACLTestConn(t, &aclRecordingSession{}, &out)
			if err := conn.writeListRights(data); err == nil {
				t.Error("writeListRights succeeded for an inconsistent tied-rights declaration")
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
