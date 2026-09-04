package imapserver

import (
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestSetACLVirtualCreateReachesSessionAsKX pins the server-side reading of RFC
// 4314 §2.1.1's virtual `c`: a client naming it delegates creating and deleting
// mailboxes (`k` `x`), the first of the two families the section describes and
// the one Dovecot and Cyrus (deleteright=c) implement. Before the fix the
// expansion was `k` alone while `d` had already stopped implying `x`, leaving
// mailbox deletion in no virtual right at all: an RFC 2086 client could neither
// grant it nor see it.
func TestSetACLVirtualCreateReachesSessionAsKX(t *testing.T) {
	cases := []struct {
		args    string
		wantMod imap.RightModification
		want    imap.RightSet
	}{
		{`INBOX bob c`, imap.RightModificationReplace, imap.RightSet("kx")},
		{`INBOX bob lrc`, imap.RightModificationReplace, imap.RightSet("lrkx")},
		{`INBOX bob +c`, imap.RightModificationAdd, imap.RightSet("kx")},
		{`INBOX bob -c`, imap.RightModificationRemove, imap.RightSet("kx")},
		// Members already named by the client are not doubled.
		{`INBOX bob kc`, imap.RightModificationReplace, imap.RightSet("kx")},
		{`INBOX bob xc`, imap.RightModificationReplace, imap.RightSet("xk")},
		// `c` and `d` together are the RFC's own example: `x` comes from `c`.
		{`INBOX bob lrswicd`, imap.RightModificationReplace, imap.RightSet("lrswikxte")},
		// `d` on its own still never confers `x`.
		{`INBOX bob lrd`, imap.RightModificationReplace, imap.RightSet("lrte")},
	}
	for _, tc := range cases {
		t.Run(tc.args, func(t *testing.T) {
			session := &aclRecordingSession{}
			conn := newACLTestConn(t, session, &bytes.Buffer{})
			if err := conn.handleSetACL(argsDecoder(" " + tc.args)); err != nil {
				t.Fatalf("handleSetACL: %v", err)
			}
			if session.setMod != tc.wantMod {
				t.Errorf("modification = %v, want %v", session.setMod, tc.wantMod)
			}
			if !session.setRights.Equal(tc.want) {
				t.Errorf("session received rights %q, want %q", session.setRights, tc.want)
			}
			// No virtual right may survive into the backend.
			for _, virt := range []imap.Right{imap.RightCreate, imap.RightDelete} {
				if containsRight(session.setRights, virt) {
					t.Errorf("rights %q: virtual %c must be expanded, not passed through", session.setRights, virt)
				}
			}
		})
	}
}

// TestACLOutputAdvertisesVirtualCreateForKX is the inverse: GETACL, MYRIGHTS and
// LISTRIGHTS append `c` whenever `k` or `x` is held (or grantable), so a bare
// `x` is visible to an RFC 2086 client through `c` and never through `d`.
func TestACLOutputAdvertisesVirtualCreateForKX(t *testing.T) {
	render := func(write func(c *Conn) error) string {
		t.Helper()
		var out bytes.Buffer
		conn := newACLTestConn(t, &aclRecordingSession{}, &out)
		if err := write(conn); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := conn.bw.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		return strings.TrimRight(out.String(), "\r\n")
	}
	lastQuoted := func(line string) string {
		t.Helper()
		j := strings.LastIndex(line, `"`)
		i := strings.LastIndex(line[:j], `"`)
		if i < 0 || j <= i {
			t.Fatalf("no quoted string in %q", line)
		}
		return line[i+1 : j]
	}

	rightsCases := []struct {
		held  imap.RightSet
		wantC bool
		wantD bool
	}{
		{imap.RightSet("lrk"), true, false},
		{imap.RightSet("lrx"), true, false},
		{imap.RightSet("lrkx"), true, false},
		{imap.RightSet("lrte"), false, true},
		{imap.RightSet("lrs"), false, false},
		// The RFC's example: lrswikda -> the client sees c and d.
		{imap.RightSet("lrswiktea"), true, true},
	}
	for _, tc := range rightsCases {
		acl := render(func(c *Conn) error {
			return c.writeGetACL(&imap.GetACLData{Mailbox: "INBOX", ACL: []imap.ACLEntry{{Identifier: "bob", Rights: tc.held}}})
		})
		my := render(func(c *Conn) error {
			return c.writeMyRights(&imap.MyRightsData{Mailbox: "INBOX", Rights: tc.held})
		})
		for name, line := range map[string]string{"GETACL": acl, "MYRIGHTS": my} {
			got := lastQuoted(line)
			if strings.ContainsRune(got, 'c') != tc.wantC {
				t.Errorf("%s for %q: %s; virtual c present = %v, want %v", name, tc.held, line, !tc.wantC, tc.wantC)
			}
			if strings.ContainsRune(got, 'd') != tc.wantD {
				t.Errorf("%s for %q: %s; virtual d present = %v, want %v", name, tc.held, line, !tc.wantD, tc.wantD)
			}
			// The compatibility rendering only ever adds the virtual letters; a
			// member right the identifier does not hold must not appear.
			for _, member := range "kx" {
				if !containsRight(tc.held, imap.Right(member)) && strings.ContainsRune(got, member) {
					t.Errorf("%s: %c rendered although not held", line, member)
				}
			}
		}
	}

	listCases := []struct {
		optional []imap.RightSet
		wantC    bool
	}{
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("k"), imap.RightSet("x")}, true},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("x")}, true},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("k")}, true},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("t")}, false},
	}
	for _, tc := range listCases {
		line := render(func(c *Conn) error {
			return c.writeListRights(&imap.ListRightsData{
				Mailbox: "INBOX", Identifier: "bob", RequiredRights: imap.RightSet{}, OptionalRights: tc.optional,
			})
		})
		if got := strings.Contains(line, ` "c"`); got != tc.wantC {
			t.Errorf("LISTRIGHTS %v: %s; virtual c group present = %v, want %v", tc.optional, line, got, tc.wantC)
		}
	}
}
