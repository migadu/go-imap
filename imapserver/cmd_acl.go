package imapserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleGetACL(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionACL)
	if !ok {
		return newClientBugError("ACL extension is not supported")
	}

	data, err := session.GetACL(c.ctx, mailbox)
	if err != nil {
		return err
	}

	return c.writeGetACL(data)
}

func (c *Conn) handleSetACL(dec *imapwire.Decoder) error {
	var mailbox, identifierStr, rightsStr string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) ||
		!dec.ExpectSP() || !dec.ExpectAString(&identifierStr) ||
		!dec.ExpectSP() || !dec.ExpectAString(&rightsStr) ||
		!dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionACL)
	if !ok {
		return newClientBugError("ACL extension is not supported")
	}

	// Parse rights modification (+ or - prefix, or replace)
	modification := imap.RightModificationReplace
	rights := imap.RightSet(rightsStr)
	if len(rightsStr) > 0 {
		switch rightsStr[0] {
		case '+':
			modification = imap.RightModificationAdd
			rights = imap.RightSet(rightsStr[1:])
		case '-':
			modification = imap.RightModificationRemove
			rights = imap.RightSet(rightsStr[1:])
		}
	}

	identifier := imap.RightsIdentifier(identifierStr)
	expanded, err := expandVirtualRights(rights, c.virtualRights())
	if err != nil {
		return err
	}
	return session.SetACL(c.ctx, mailbox, identifier, modification, expanded)
}

func (c *Conn) handleDeleteACL(dec *imapwire.Decoder) error {
	var mailbox, identifierStr string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) ||
		!dec.ExpectSP() || !dec.ExpectAString(&identifierStr) ||
		!dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionACL)
	if !ok {
		return newClientBugError("ACL extension is not supported")
	}

	identifier := imap.RightsIdentifier(identifierStr)
	return session.DeleteACL(c.ctx, mailbox, identifier)
}

func (c *Conn) handleListRights(dec *imapwire.Decoder) error {
	var mailbox, identifierStr string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) ||
		!dec.ExpectSP() || !dec.ExpectAString(&identifierStr) ||
		!dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionACL)
	if !ok {
		return newClientBugError("ACL extension is not supported")
	}

	identifier := imap.RightsIdentifier(identifierStr)
	data, err := session.ListRights(c.ctx, mailbox, identifier)
	if err != nil {
		return err
	}

	return c.writeListRights(data)
}

func (c *Conn) handleMyRights(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionACL)
	if !ok {
		return newClientBugError("ACL extension is not supported")
	}

	data, err := session.MyRights(c.ctx, mailbox)
	if err != nil {
		return err
	}

	return c.writeMyRights(data)
}

func (c *Conn) writeGetACL(data *imap.GetACLData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	vr := c.virtualRights()
	enc.Atom("*").SP().Atom("ACL").SP().Mailbox(data.Mailbox)
	for i := range data.ACL {
		entry := &data.ACL[i]
		enc.SP().String(string(entry.Identifier)).SP().String(formatRightsWithCompat(entry.Rights, vr))
	}
	return enc.CRLF()
}

func (c *Conn) writeListRights(data *imap.ListRightsData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("LISTRIGHTS").SP().
		Mailbox(data.Mailbox).SP().
		String(string(data.Identifier)).SP().
		String(string(data.RequiredRights))

	// Write optional rights groups verbatim. Rights in the same group are "tied"
	// (RFC 4314 §3.7: all-or-none), so the caller controls grouping; we must not
	// merge the obsolete c/d into them here.
	for i := range data.OptionalRights {
		enc.SP().String(string(data.OptionalRights[i]))
	}

	// RFC 4314 §2.1.1: if the identifier can be granted any member of a virtual
	// right, that obsolete right MUST be advertised. The members here are listed
	// individually (each its own group, independently grantable), so the virtual
	// right is returned by itself as its own group. §3.7 forbids listing any right
	// more than once, so only add c/d when not already present. Which rights are
	// members is the session's declaration (see virtualRights), the same one
	// SETACL expands against.
	var all imap.RightSet
	all = append(all, data.RequiredRights...)
	for i := range data.OptionalRights {
		all = append(all, data.OptionalRights[i]...)
	}
	vr := c.virtualRights()
	if anyRightIn(vr.create, all) && !containsRight(all, imap.RightCreate) {
		enc.SP().String("c")
	}
	if anyRightIn(vr.delete, all) && !containsRight(all, imap.RightDelete) {
		enc.SP().String("d")
	}

	return enc.CRLF()
}

func (c *Conn) writeMyRights(data *imap.MyRightsData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("MYRIGHTS").SP().
		Mailbox(data.Mailbox).SP().
		String(formatRightsWithCompat(data.Rights, c.virtualRights()))

	return enc.CRLF()
}

// For backwards compatibility, keep the old SETACL format helper
func formatRights(rm imap.RightModification, rs imap.RightSet) string {
	return internal.FormatRights(rm, rs)
}

// virtualRights is the resolved membership of RFC 4314 §2.1.1's virtual `c`
// and `d` rights for one connection. It is the single source that
// expandVirtualRights, formatRightsWithCompat and writeListRights all read, so
// what SETACL accepts on the session's behalf and what GETACL, MYRIGHTS and
// LISTRIGHTS advertise on its behalf cannot drift apart.
type virtualRights struct {
	create imap.RightSet
	delete imap.RightSet
}

// virtualRights returns the session's declaration when it makes one
// (SessionACLVirtualRights) and the RFC's first family otherwise.
func (c *Conn) virtualRights() virtualRights {
	if s, ok := c.session.(SessionACLVirtualRights); ok {
		create, delete := s.VirtualRights()
		return newVirtualRights(create, delete)
	}
	return defaultVirtualRights()
}

func defaultVirtualRights() virtualRights {
	return newVirtualRights(DefaultVirtualCreate, DefaultVirtualDelete)
}

// newVirtualRights normalizes a declaration: the virtual letters themselves
// are not members of anything, a right is listed once, and a right named in
// both sets is kept in `create` only. The last rule is what stops a backend
// from putting `x` in both, which no RFC family does and which would make
// every output append both `c` and `d` for it.
func newVirtualRights(create, delete imap.RightSet) virtualRights {
	var vr virtualRights
	for _, r := range create {
		if r != imap.RightCreate && r != imap.RightDelete && !containsRight(vr.create, r) {
			vr.create = append(vr.create, r)
		}
	}
	for _, r := range delete {
		if r != imap.RightCreate && r != imap.RightDelete && !containsRight(vr.create, r) && !containsRight(vr.delete, r) {
			vr.delete = append(vr.delete, r)
		}
	}
	return vr
}

// expandVirtualRights maps the obsolete RFC 2086 rights a client may still name
// (RFC 4314 §2.1.1) onto their members, as the session declares them: by
// default `c` becomes `k` `x` and `d` becomes `t` `e`.
//
// §2.1.1 describes two server families and lets each define the virtual
// rights: servers whose RFC 2086 `c` controlled DELETE read `c` as `k`+`x` and
// `d` as `e`+`t`; servers whose `d` controlled DELETE read `c` as `k` and `d` as
// `e`+`t`+`x`. The default is the first family, as are Dovecot (unconditionally)
// and Cyrus (its default, deleteright=c): the RFC's own worked examples expand
// `d` to `et`, and a client written against either expects "SETACL ... d" to
// delegate deleting and expunging MESSAGES and not the mailbox itself. A
// backend for which `x` is never grantable declares it a member of neither
// (SessionACLVirtualRights), so that a client's `c` does not turn into a
// request the backend must refuse over a letter the client never typed.
//
// A member the client also named explicitly is not doubled, and an explicit
// non-member (an `x` under a backend that keeps it out of both) is passed
// through as the client's own request.
//
// A virtual right the session declares with no members is not a right this
// server has at all, and RFC 4314 §3.1 requires an unrecognized right to draw a
// BAD rather than be ignored: naming it returns a client-bug error, so that
// "SETACL ... c" against such a backend cannot quietly turn into an empty
// replacement that revokes the identifier's entry.
//
// formatRightsWithCompat and writeListRights are the inverse and read the same
// declaration: they append `c` when any create member is held (or grantable)
// and `d` when any delete member is.
func expandVirtualRights(rs imap.RightSet, vr virtualRights) (imap.RightSet, error) {
	res := make(imap.RightSet, 0, len(rs)+len(vr.create)+len(vr.delete))
	hasC := false
	hasD := false
	for _, r := range rs {
		switch r {
		case imap.RightCreate:
			hasC = true
		case imap.RightDelete:
			hasD = true
		default:
			res = append(res, r)
		}
	}
	if hasC {
		if len(vr.create) == 0 {
			return nil, newClientBugError("The c right is not supported")
		}
		res = appendMissing(res, vr.create)
	}
	if hasD {
		if len(vr.delete) == 0 {
			return nil, newClientBugError("The d right is not supported")
		}
		res = appendMissing(res, vr.delete)
	}
	return res, nil
}

func appendMissing(rs, add imap.RightSet) imap.RightSet {
	for _, r := range add {
		if !containsRight(rs, r) {
			rs = append(rs, r)
		}
	}
	return rs
}

func containsRight(rs imap.RightSet, r imap.Right) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// anyRightIn reports whether any of members is present in rs.
func anyRightIn(members, rs imap.RightSet) bool {
	for _, m := range members {
		if containsRight(rs, m) {
			return true
		}
	}
	return false
}

// formatRightsWithCompat renders a right set for GETACL/MYRIGHTS with the
// obsolete virtual rights appended (RFC 4314 §2.1.1): `c` when any member of
// the virtual create right is held, `d` when any member of the virtual delete
// right is. Membership is the session's declaration, the same one
// expandVirtualRights reads; by default `x` alone implies `c`, not `d`.
func formatRightsWithCompat(rs imap.RightSet, vr virtualRights) string {
	s := string(rs)
	if anyRightIn(vr.create, rs) && !containsRight(rs, imap.RightCreate) {
		s += "c"
	}
	if anyRightIn(vr.delete, rs) && !containsRight(rs, imap.RightDelete) {
		s += "d"
	}
	return s
}
