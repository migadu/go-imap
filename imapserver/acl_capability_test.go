package imapserver

import (
	"context"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// aclCapSession is a minimal session that implements SessionACL. The embedded
// Session interface is nil and must never be called; availableCaps() only needs
// the type to satisfy SessionACL via a type assertion.
type aclCapSession struct {
	Session
}

func (aclCapSession) GetACL(ctx context.Context, mailbox string) (*imap.GetACLData, error) {
	return nil, nil
}
func (aclCapSession) SetACL(ctx context.Context, mailbox string, id imap.RightsIdentifier, mod imap.RightModification, rights imap.RightSet) error {
	return nil
}
func (aclCapSession) DeleteACL(ctx context.Context, mailbox string, id imap.RightsIdentifier) error {
	return nil
}
func (aclCapSession) ListRights(ctx context.Context, mailbox string, id imap.RightsIdentifier) (*imap.ListRightsData, error) {
	return nil, nil
}
func (aclCapSession) MyRights(ctx context.Context, mailbox string) (*imap.MyRightsData, error) {
	return nil, nil
}

func hasCap(caps []imap.Cap, want imap.Cap) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// TestACLCapabilityAdvertisesRights verifies RFC 4314 compliance: a server whose
// session implements the ACL extension must advertise both "ACL" and the
// "RIGHTS=" capability. Per Section 2.1 the RIGHTS= string must contain "t",
// "e", "x", and "k"; per Section 2.2 it must not contain RFC 2086 rights.
func TestACLCapabilityAdvertisesRights(t *testing.T) {
	srv := &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}}

	for _, state := range []imap.ConnState{imap.ConnStateAuthenticated, imap.ConnStateSelected} {
		conn := &Conn{server: srv, state: state, session: aclCapSession{}}
		caps := conn.availableCaps()

		if !hasCap(caps, imap.CapACL) {
			t.Errorf("state %v: ACL capability not advertised, got %v", state, caps)
		}

		rightsCap := imap.Cap("RIGHTS=" + imap.RightSetExtended.String())
		if !hasCap(caps, rightsCap) {
			t.Errorf("state %v: %q not advertised, got %v", state, rightsCap, caps)
		}

		// Section 2.1: RIGHTS= MUST include t, e, x, k.
		rights := imap.RightSetExtended.String()
		for _, r := range []rune{'t', 'e', 'x', 'k'} {
			if !containsRune(rights, r) {
				t.Errorf("RIGHTS=%q missing required right %q (RFC 4314 2.1)", rights, r)
			}
		}
		// Section 2.2: RIGHTS= MUST NOT include RFC 2086 rights or digits.
		for _, r := range "lrswipacd0123456789" {
			if containsRune(rights, r) {
				t.Errorf("RIGHTS=%q must not include RFC 2086 right %q (RFC 4314 2.2)", rights, r)
			}
		}
	}
}

// TestNonACLSessionOmitsRights verifies a session without ACL support advertises
// neither ACL nor RIGHTS=.
func TestNonACLSessionOmitsRights(t *testing.T) {
	srv := &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}}
	conn := &Conn{server: srv, state: imap.ConnStateAuthenticated, session: struct{ Session }{}}

	caps := conn.availableCaps()
	if hasCap(caps, imap.CapACL) {
		t.Errorf("ACL advertised for non-ACL session: %v", caps)
	}
	for _, c := range caps {
		if len(c) >= 7 && c[:7] == "RIGHTS=" {
			t.Errorf("RIGHTS= advertised for non-ACL session: %v", c)
		}
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestExpandVirtualRights(t *testing.T) {
	tests := []struct {
		input imap.RightSet
		want  imap.RightSet
	}{
		{imap.RightSet("lrswi"), imap.RightSet("lrswi")},
		// `c` is `k`+`x` and `d` is `t`+`e` (RFC 4314 §2.1.1 first family, as
		// Dovecot and Cyrus' default): mailbox deletion `x` comes with `c`, and
		// `d` never implies it.
		{imap.RightSet("c"), imap.RightSet("kx")},
		{imap.RightSet("d"), imap.RightSet("te")},
		{imap.RightSet("cd"), imap.RightSet("kxte")},
		{imap.RightSet("lrswicda"), imap.RightSet("lrswikxtea")},
		// An explicit `x` survives alongside the expansion, and is not doubled.
		{imap.RightSet("xd"), imap.RightSet("xte")},
		{imap.RightSet("xc"), imap.RightSet("xk")},
		{imap.RightSet("td"), imap.RightSet("te")},
	}

	for _, tc := range tests {
		got := expandVirtualRights(tc.input)
		if !got.Equal(tc.want) {
			t.Errorf("expandVirtualRights(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFormatRightsWithCompat(t *testing.T) {
	tests := []struct {
		input imap.RightSet
		want  string
	}{
		{imap.RightSet("lrswi"), "lrswi"},
		{imap.RightSet("k"), "kc"},
		// `x` alone is a member of the virtual `c`, not of `d`.
		{imap.RightSet("x"), "xc"},
		{imap.RightSet("t"), "td"},
		{imap.RightSet("e"), "ed"},
		{imap.RightSet("te"), "ted"},
		{imap.RightSet("kxte"), "kxtecd"},
	}

	for _, tc := range tests {
		got := formatRightsWithCompat(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("formatRightsWithCompat(%q) = %q, want %q", tc.input, got, tc.want)
			continue
		}
		for _, r := range got {
			if !containsRune(tc.want, r) {
				t.Errorf("formatRightsWithCompat(%q) = %q, want %q", tc.input, got, tc.want)
				break
			}
		}
	}
}
