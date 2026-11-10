package imapserver

import (
	"fmt"

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
	required, optional, err := listRightsWithCompat(data, c.virtualRights())
	if err != nil {
		return err
	}

	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("LISTRIGHTS").SP().
		Mailbox(data.Mailbox).SP().
		String(string(data.Identifier)).SP().
		String(required)
	for _, group := range optional {
		enc.SP().String(group)
	}
	return enc.CRLF()
}

// listRightsWithCompat returns the required string and the optional groups of
// a LISTRIGHTS response with the virtual rights placed as RFC 4314 §2.1.1
// asks. It never changes the backend's real rights or ties, and it runs before
// anything is encoded, so that a layout it has to refuse cannot leave a
// partial LISTRIGHTS response on the wire.
//
// The virtual rights are derived, not taken from the session: a `c` or `d` in
// data is dropped and the letter is placed from the declaration alone, the one
// SETACL expands against. Only the members the identifier is offered count. A
// member that is neither required nor optional cannot be granted, and the
// RFC's own first example advertises `d` without `e`. For each virtual right:
//
//   - No member is offered: it is not written.
//   - Every offered member is required: it is always granted too, so it joins
//     the required string (§3.7).
//   - No optional member shares a group with a non-member: it "is returned by
//     itself", as a group of its own. That includes members split between
//     required and optional, since granting it grants the optional ones.
//   - An optional member is tied to a non-member: the letter and "all of the
//     member rights need to be tied to the same non-member right(s)". If one
//     group holds every optional member, the letter joins that group. If they
//     are spread over several, the RFC does not allow the layout and it is
//     refused: merging the groups here would make an all-or-none promise
//     (§3.7) that the backend's SETACL need not keep.
func listRightsWithCompat(data *imap.ListRightsData, vr virtualRights) (string, []string, error) {
	required := withoutVirtualRights(data.RequiredRights)
	var groups []imap.RightSet
	for _, group := range data.OptionalRights {
		if group = withoutVirtualRights(group); len(group) > 0 {
			groups = append(groups, group)
		}
	}

	requiredOut := string(required)
	optional := make([]string, len(groups))
	for i, group := range groups {
		optional[i] = string(group)
	}
	var standalone []string
	addVirtual := func(right imap.Right, members imap.RightSet) error {
		// The optional groups holding a member, and whether any of them ties
		// it to a non-member.
		var holding []int
		tied := false
		for i, group := range groups {
			if !anyRightIn(members, group) {
				continue
			}
			holding = append(holding, i)
			if !allRightsIn(group, members) {
				tied = true
			}
		}
		switch {
		case len(holding) == 0:
			if anyRightIn(members, required) {
				requiredOut += string(right)
			}
		case !tied:
			standalone = append(standalone, string(right))
		case len(holding) == 1:
			optional[holding[0]] += string(right)
		default:
			split := make([]string, len(holding))
			for i, g := range holding {
				split[i] = string(groups[g])
			}
			return fmt.Errorf("imapserver: LISTRIGHTS %q: members %q of virtual %c are tied to other rights but split over optional groups %q", data.Mailbox, string(members), right, split)
		}
		return nil
	}
	if err := addVirtual(imap.RightCreate, vr.create); err != nil {
		return "", nil, err
	}
	if err := addVirtual(imap.RightDelete, vr.delete); err != nil {
		return "", nil, err
	}
	return requiredOut, append(optional, standalone...), nil
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
// declaration: they add `c` when any create member is held (or grantable) and
// `d` when any delete member is. Where LISTRIGHTS puts the letter is
// listRightsWithCompat's business.
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

// withoutVirtualRights returns a copy of rs without the virtual `c` and `d`.
func withoutVirtualRights(rs imap.RightSet) imap.RightSet {
	res := make(imap.RightSet, 0, len(rs))
	for _, r := range rs {
		if r != imap.RightCreate && r != imap.RightDelete {
			res = append(res, r)
		}
	}
	return res
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

// allRightsIn reports whether every one of members is present in rs. It is true
// for empty members, so a caller asks anyRightIn first.
func allRightsIn(members, rs imap.RightSet) bool {
	for _, m := range members {
		if !containsRight(rs, m) {
			return false
		}
	}
	return true
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
