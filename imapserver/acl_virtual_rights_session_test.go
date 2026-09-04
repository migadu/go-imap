package imapserver

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// aclFamilySession is a SessionACL that declares its own virtual-right
// membership through SessionACLVirtualRights.
type aclFamilySession struct {
	aclRecordingSession
	create imap.RightSet
	delete imap.RightSet
}

func (s *aclFamilySession) VirtualRights() (create, delete imap.RightSet) {
	return s.create, s.delete
}

var _ SessionACLVirtualRights = (*aclFamilySession)(nil)

// TestNewVirtualRightsNormalizes pins the normalization every consumer relies
// on: the virtual letters are not members of anything, a right is listed once,
// and a right named in both sets belongs to `create` only. A backend therefore
// cannot put `x` in both, which no RFC 4314 §2.1.1 family does.
func TestNewVirtualRightsNormalizes(t *testing.T) {
	cases := []struct {
		create, delete         imap.RightSet
		wantCreate, wantDelete imap.RightSet
	}{
		{imap.RightSet("kx"), imap.RightSet("te"), imap.RightSet("kx"), imap.RightSet("te")},
		// The second RFC family.
		{imap.RightSet("k"), imap.RightSet("xte"), imap.RightSet("k"), imap.RightSet("xte")},
		// A backend that grants `x` to nobody keeps it out of both.
		{imap.RightSet("k"), imap.RightSet("te"), imap.RightSet("k"), imap.RightSet("te")},
		// `x` in both: create wins.
		{imap.RightSet("kx"), imap.RightSet("xte"), imap.RightSet("kx"), imap.RightSet("te")},
		// The virtual letters and duplicates are dropped.
		{imap.RightSet("kxck"), imap.RightSet("tded"), imap.RightSet("kx"), imap.RightSet("te")},
		{nil, nil, nil, nil},
	}
	for _, tc := range cases {
		got := newVirtualRights(tc.create, tc.delete)
		if !got.create.Equal(tc.wantCreate) || len(got.create) != len(tc.wantCreate) {
			t.Errorf("newVirtualRights(%q, %q).create = %q, want %q", tc.create, tc.delete, got.create, tc.wantCreate)
		}
		if !got.delete.Equal(tc.wantDelete) || len(got.delete) != len(tc.wantDelete) {
			t.Errorf("newVirtualRights(%q, %q).delete = %q, want %q", tc.create, tc.delete, got.delete, tc.wantDelete)
		}
	}
}

// TestExpandVirtualRightsFollowsDeclaration checks the expansion against each
// family a backend may declare, including the one where `x` is a member of
// neither: there a client's `c` reaches the session as `k` alone and an
// explicit `x` still passes through as the client's own request.
func TestExpandVirtualRightsFollowsDeclaration(t *testing.T) {
	noX := newVirtualRights(imap.RightSet("k"), imap.RightSet("te"))
	second := newVirtualRights(imap.RightSet("k"), imap.RightSet("xte"))

	cases := []struct {
		vr    virtualRights
		input imap.RightSet
		want  imap.RightSet
	}{
		{noX, imap.RightSet("c"), imap.RightSet("k")},
		{noX, imap.RightSet("d"), imap.RightSet("te")},
		{noX, imap.RightSet("lrswicd"), imap.RightSet("lrswikte")},
		{noX, imap.RightSet("xc"), imap.RightSet("xk")},
		{second, imap.RightSet("c"), imap.RightSet("k")},
		{second, imap.RightSet("d"), imap.RightSet("xte")},
		{second, imap.RightSet("lrswicd"), imap.RightSet("lrswikxte")},
	}
	for _, tc := range cases {
		got, err := expandVirtualRights(tc.input, tc.vr)
		if err != nil {
			t.Errorf("expandVirtualRights(%q, c=%q d=%q): %v", tc.input, tc.vr.create, tc.vr.delete, err)
			continue
		}
		if !got.Equal(tc.want) || len(got) != len(tc.want) {
			t.Errorf("expandVirtualRights(%q, c=%q d=%q) = %q, want %q", tc.input, tc.vr.create, tc.vr.delete, got, tc.want)
		}
	}
}

// TestMemberlessVirtualRightIsRefused pins RFC 4314 §3.1 for a virtual right
// the session declares without members: it is not a right this server has, so
// naming it is a client bug (BAD), never an empty expansion. Before the fix
// "SETACL INBOX bob c" under such a declaration reached the session as an empty
// Replace and silently revoked bob's entry. Explicit member letters are
// unaffected, and a declaration that empties only one virtual right refuses
// only that one.
func TestMemberlessVirtualRightIsRefused(t *testing.T) {
	none := newVirtualRights(nil, nil)
	noDelete := newVirtualRights(imap.RightSet("kx"), nil)

	cases := []struct {
		vr      virtualRights
		input   imap.RightSet
		want    imap.RightSet
		wantBad bool
	}{
		{none, imap.RightSet("c"), nil, true},
		{none, imap.RightSet("lrd"), nil, true},
		{none, imap.RightSet("lrkx"), imap.RightSet("lrkx"), false},
		{noDelete, imap.RightSet("lrc"), imap.RightSet("lrkx"), false},
		{noDelete, imap.RightSet("lrd"), nil, true},
	}
	for _, tc := range cases {
		got, err := expandVirtualRights(tc.input, tc.vr)
		if tc.wantBad {
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBad {
				t.Errorf("expandVirtualRights(%q, c=%q d=%q) = %q, %v; want a BAD error", tc.input, tc.vr.create, tc.vr.delete, got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("expandVirtualRights(%q, c=%q d=%q): %v", tc.input, tc.vr.create, tc.vr.delete, err)
			continue
		}
		if !got.Equal(tc.want) || len(got) != len(tc.want) {
			t.Errorf("expandVirtualRights(%q, c=%q d=%q) = %q, want %q", tc.input, tc.vr.create, tc.vr.delete, got, tc.want)
		}
	}

	// Through the handler: the session must not be called at all.
	session := &aclFamilySession{}
	conn := newACLTestConn(t, session, &bytes.Buffer{})
	err := conn.handleSetACL(argsDecoder(" INBOX bob lrc"))
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBad {
		t.Fatalf("handleSetACL under an empty declaration returned %v, want BAD", err)
	}
	if session.calls != 0 {
		t.Errorf("session.SetACL was called %d time(s) with rights %q; a refused SETACL must not reach the backend", session.calls, session.setRights)
	}
}

// TestFormatRightsWithCompatFollowsDeclaration is the inverse: what GETACL and
// MYRIGHTS append depends on the same declaration, so a backend that keeps `x`
// out of `c` never renders `c` for an `x`, and the second family renders `d`
// for it instead.
func TestFormatRightsWithCompatFollowsDeclaration(t *testing.T) {
	noX := newVirtualRights(imap.RightSet("k"), imap.RightSet("te"))
	second := newVirtualRights(imap.RightSet("k"), imap.RightSet("xte"))
	none := newVirtualRights(nil, nil)

	cases := []struct {
		vr    virtualRights
		input imap.RightSet
		want  string
	}{
		{noX, imap.RightSet("k"), "kc"},
		{noX, imap.RightSet("x"), "x"},
		{noX, imap.RightSet("kx"), "kxc"},
		{noX, imap.RightSet("te"), "ted"},
		{noX, imap.RightSet("lrswikxtea"), "lrswikxteacd"},
		{second, imap.RightSet("x"), "xd"},
		{second, imap.RightSet("k"), "kc"},
		{none, imap.RightSet("kxte"), "kxte"},
		// A virtual letter already present is not doubled.
		{noX, imap.RightSet("kc"), "kc"},
	}
	for _, tc := range cases {
		if got := formatRightsWithCompat(tc.input, tc.vr); got != tc.want {
			t.Errorf("formatRightsWithCompat(%q, c=%q d=%q) = %q, want %q", tc.input, tc.vr.create, tc.vr.delete, got, tc.want)
		}
	}
}

// TestSessionDeclaredVirtualRightsReachAllThreeConsumers drives a session that
// declares `c` = `k` and `d` = `t`+`e` through the handler and the three
// writers, and checks the wire agrees with itself: SETACL expands `c` to `k`
// only, GETACL and MYRIGHTS append `c` for `k` and not for `x`, and LISTRIGHTS
// adds the `c` group when `k` is grantable and not when only `x` is.
func TestSessionDeclaredVirtualRightsReachAllThreeConsumers(t *testing.T) {
	newSession := func() *aclFamilySession {
		return &aclFamilySession{create: imap.RightSet("k"), delete: imap.RightSet("te")}
	}

	setCases := []struct {
		args string
		want imap.RightSet
	}{
		{`INBOX bob c`, imap.RightSet("k")},
		{`INBOX bob lrc`, imap.RightSet("lrk")},
		{`INBOX bob +c`, imap.RightSet("k")},
		{`INBOX bob kc`, imap.RightSet("k")},
		{`INBOX bob lrswicd`, imap.RightSet("lrswikte")},
		// An explicit `x` is the client's own and is passed through.
		{`INBOX bob xc`, imap.RightSet("xk")},
	}
	for _, tc := range setCases {
		t.Run(tc.args, func(t *testing.T) {
			session := newSession()
			conn := newACLTestConn(t, session, &bytes.Buffer{})
			if err := conn.handleSetACL(argsDecoder(" " + tc.args)); err != nil {
				t.Fatalf("handleSetACL: %v", err)
			}
			if !session.setRights.Equal(tc.want) || len(session.setRights) != len(tc.want) {
				t.Errorf("session received rights %q, want %q", session.setRights, tc.want)
			}
		})
	}

	render := func(write func(c *Conn) error) string {
		t.Helper()
		var out bytes.Buffer
		conn := newACLTestConn(t, newSession(), &out)
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

	outCases := []struct {
		held imap.RightSet
		want string
	}{
		{imap.RightSet("lrk"), "lrkc"},
		{imap.RightSet("lrx"), "lrx"},
		{imap.RightSet("lrkx"), "lrkxc"},
		{imap.RightSet("lrte"), "lrted"},
		// An owner holding everything this server implements.
		{imap.RightSet("lrswikxtea"), "lrswikxteacd"},
	}
	for _, tc := range outCases {
		acl := render(func(c *Conn) error {
			return c.writeGetACL(&imap.GetACLData{Mailbox: "INBOX", ACL: []imap.ACLEntry{{Identifier: "bob", Rights: tc.held}}})
		})
		my := render(func(c *Conn) error {
			return c.writeMyRights(&imap.MyRightsData{Mailbox: "INBOX", Rights: tc.held})
		})
		for name, line := range map[string]string{"GETACL": acl, "MYRIGHTS": my} {
			if got := lastQuoted(line); got != tc.want {
				t.Errorf("%s for %q rendered %q, want %q: %s", name, tc.held, got, tc.want, line)
			}
		}
	}

	listCases := []struct {
		optional []imap.RightSet
		wantC    bool
		wantD    bool
	}{
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("k")}, true, false},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("x")}, false, false},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("t")}, false, true},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("k"), imap.RightSet("t"), imap.RightSet("e")}, true, true},
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
		if got := strings.Contains(line, ` "d"`); got != tc.wantD {
			t.Errorf("LISTRIGHTS %v: %s; virtual d group present = %v, want %v", tc.optional, line, got, tc.wantD)
		}
	}
}

// TestSessionWithoutDeclarationGetsTheDefaultFamily: a plain SessionACL is
// served exactly as before this interface existed.
func TestSessionWithoutDeclarationGetsTheDefaultFamily(t *testing.T) {
	conn := newACLTestConn(t, &aclRecordingSession{}, &bytes.Buffer{})
	vr := conn.virtualRights()
	if !vr.create.Equal(imap.RightSet("kx")) || !vr.delete.Equal(imap.RightSet("te")) {
		t.Errorf("default virtual rights c=%q d=%q, want c=kx d=te", vr.create, vr.delete)
	}
}
