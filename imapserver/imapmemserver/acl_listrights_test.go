package imapmemserver

import (
	"context"
	"testing"

	"github.com/migadu/go-imap"
)

func TestListRightsAdvertisesIndependentGrants(t *testing.T) {
	ctx := context.Background()
	sess := NewUserSession(NewUser("alice", "secret"))
	if err := sess.Create(ctx, "INBOX", nil); err != nil {
		t.Fatal(err)
	}

	const identifier = imap.RightsIdentifier("bob")
	data, err := sess.ListRights(ctx, "INBOX", identifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.RequiredRights) != 0 {
		t.Errorf("required rights = %q, want none", data.RequiredRights)
	}
	if len(data.OptionalRights) != len(imap.RightSetAll) {
		t.Fatalf("optional groups = %q, want one per standard right", data.OptionalRights)
	}

	for i, right := range imap.RightSetAll {
		group := data.OptionalRights[i]
		if len(group) != 1 || group[0] != right {
			t.Errorf("optional group %d = %q, want %q", i, group, string(right))
		}
		if err := sess.SetACL(ctx, "INBOX", identifier, imap.RightModificationReplace, imap.RightSet{right}); err != nil {
			t.Fatalf("grant %q: %v", string(right), err)
		}
		acl, err := sess.GetACL(ctx, "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		var granted imap.RightSet
		for _, entry := range acl.ACL {
			if entry.Identifier == identifier {
				granted = entry.Rights
			}
		}
		if len(granted) != 1 || granted[0] != right {
			t.Errorf("grant %q produced %q, want only that right", string(right), granted)
		}
	}
}

func TestListRightsOptionalGroupsAreOwnedByResponse(t *testing.T) {
	ctx := context.Background()
	sess := NewUserSession(NewUser("alice", "secret"))
	if err := sess.Create(ctx, "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	want := append(imap.RightSet(nil), imap.RightSetAll...)
	t.Cleanup(func() { copy(imap.RightSetAll, want) })

	listRights := func() *imap.ListRightsData {
		t.Helper()
		data, err := sess.ListRights(ctx, "INBOX", "bob")
		if err != nil {
			t.Fatal(err)
		}
		if len(data.OptionalRights) != len(want) {
			t.Fatalf("optional groups = %q, want one per standard right", data.OptionalRights)
		}
		return data
	}
	first, second := listRights(), listRights()
	first.OptionalRights[0][0] = '0'
	if string(imap.RightSetAll) != string(want) {
		t.Errorf("mutating response changed RightSetAll to %q", imap.RightSetAll)
	}
	for _, data := range []*imap.ListRightsData{second, listRights()} {
		for i, group := range data.OptionalRights {
			if len(group) != 1 || group[0] != want[i] {
				t.Errorf("another response's group %d = %q, want %q", i, group, string(want[i]))
			}
		}
	}
}
