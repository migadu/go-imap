package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// aclRecordingSession records the rights SETACL hands the backend and serves
// canned data to the GETACL/MYRIGHTS/LISTRIGHTS writers. The embedded Session is
// nil and must never be called.
type aclRecordingSession struct {
	Session
	setRights imap.RightSet
	setMod    imap.RightModification
	calls     int
}

func (s *aclRecordingSession) GetACL(ctx context.Context, mailbox string) (*imap.GetACLData, error) {
	return nil, nil
}
func (s *aclRecordingSession) SetACL(ctx context.Context, mailbox string, id imap.RightsIdentifier, mod imap.RightModification, rights imap.RightSet) error {
	s.calls++
	s.setMod, s.setRights = mod, rights
	return nil
}
func (s *aclRecordingSession) DeleteACL(ctx context.Context, mailbox string, id imap.RightsIdentifier) error {
	return nil
}
func (s *aclRecordingSession) ListRights(ctx context.Context, mailbox string, id imap.RightsIdentifier) (*imap.ListRightsData, error) {
	return nil, nil
}
func (s *aclRecordingSession) MyRights(ctx context.Context, mailbox string) (*imap.MyRightsData, error) {
	return nil, nil
}

// newACLTestConn returns an authenticated Conn whose responses land in out.
func newACLTestConn(t *testing.T, session Session, out *bytes.Buffer) *Conn {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	return &Conn{
		conn:    c1,
		session: session,
		state:   imap.ConnStateAuthenticated,
		server:  New(&Options{}),
		bw:      bufio.NewWriter(out),
	}
}

func argsDecoder(args string) *imapwire.Decoder {
	return imapwire.NewDecoder(bufio.NewReader(strings.NewReader(args+"\r\n")), imapwire.ConnSideServer)
}

// TestSetACLVirtualDeleteReachesSessionAsTE pins the server-side reading of RFC
// 4314 §2.1.1's virtual `d`: a client naming it delegates deleting and expunging
// messages (`t` `e`), as Dovecot and Cyrus define it, and never mailbox deletion
// (`x`). Before the fix the expansion was `x t e`, so a backend that cannot
// grant `x` had to refuse "SETACL ... d" over a letter the client never typed.
func TestSetACLVirtualDeleteReachesSessionAsTE(t *testing.T) {
	cases := []struct {
		args    string
		wantMod imap.RightModification
		want    imap.RightSet
	}{
		{`INBOX bob d`, imap.RightModificationReplace, imap.RightSet("te")},
		{`INBOX bob lrd`, imap.RightModificationReplace, imap.RightSet("lrte")},
		{`INBOX bob +d`, imap.RightModificationAdd, imap.RightSet("te")},
		{`INBOX bob -d`, imap.RightModificationRemove, imap.RightSet("te")},
		// An explicit `x` is the client's own and is passed through.
		{`INBOX bob xd`, imap.RightModificationReplace, imap.RightSet("xte")},
		{`INBOX bob x`, imap.RightModificationReplace, imap.RightSet("x")},
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
			if containsRight(session.setRights, imap.RightDeleteMbox) != containsRight(tc.want, imap.RightDeleteMbox) {
				t.Errorf("rights %q: `x` presence must follow the client's request, not the virtual `d`", session.setRights)
			}
		})
	}
}

// TestACLOutputAdvertisesVirtualDeleteForTE is the inverse: GETACL, MYRIGHTS and
// LISTRIGHTS append `d` whenever `t` or `e` is held (or grantable), and not for
// a bare `x`, so what the server advertises is exactly what it accepts.
func TestACLOutputAdvertisesVirtualDeleteForTE(t *testing.T) {
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

	// Each rendered rights string is a quoted string; lastQuoted returns the
	// content of the last one on the line.
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
		wantD bool
	}{
		{imap.RightSet("lrte"), true},
		{imap.RightSet("lrt"), true},
		{imap.RightSet("lre"), true},
		{imap.RightSet("lrx"), false},
		{imap.RightSet("lrs"), false},
	}
	for _, tc := range rightsCases {
		acl := render(func(c *Conn) error {
			return c.writeGetACL(&imap.GetACLData{Mailbox: "INBOX", ACL: []imap.ACLEntry{{Identifier: "bob", Rights: tc.held}}})
		})
		if got := strings.ContainsRune(lastQuoted(acl), 'd'); got != tc.wantD {
			t.Errorf("GETACL for %q: %s; virtual d present = %v, want %v", tc.held, acl, got, tc.wantD)
		}
		my := render(func(c *Conn) error {
			return c.writeMyRights(&imap.MyRightsData{Mailbox: "INBOX", Rights: tc.held})
		})
		if got := strings.ContainsRune(lastQuoted(my), 'd'); got != tc.wantD {
			t.Errorf("MYRIGHTS for %q: %s; virtual d present = %v, want %v", tc.held, my, got, tc.wantD)
		}
		// A right the identifier does not hold must never be added by the
		// compatibility rendering.
		for _, line := range []string{acl, my} {
			if !containsRight(tc.held, imap.RightDeleteMbox) && strings.ContainsRune(lastQuoted(line), 'x') {
				t.Errorf("%s: `x` rendered although not held", line)
			}
		}
	}

	listCases := []struct {
		optional []imap.RightSet
		wantD    bool
	}{
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("t"), imap.RightSet("e")}, true},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("e")}, true},
		{[]imap.RightSet{imap.RightSet("l"), imap.RightSet("x")}, false},
	}
	for _, tc := range listCases {
		line := render(func(c *Conn) error {
			return c.writeListRights(&imap.ListRightsData{
				Mailbox: "INBOX", Identifier: "bob", RequiredRights: imap.RightSet{}, OptionalRights: tc.optional,
			})
		})
		if got := strings.Contains(line, ` "d"`); got != tc.wantD {
			t.Errorf("LISTRIGHTS %v: %s; virtual d group present = %v, want %v", tc.optional, line, got, tc.wantD)
		}
	}
}
