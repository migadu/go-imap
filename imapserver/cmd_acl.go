package imapserver

import (
	"strings"

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
	return session.SetACL(c.ctx, mailbox, identifier, modification, expandVirtualRights(rights))
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

	enc.Atom("*").SP().Atom("ACL").SP().Mailbox(data.Mailbox)
	for i := range data.ACL {
		entry := &data.ACL[i]
		enc.SP().String(string(entry.Identifier)).SP().String(formatRightsWithCompat(entry.Rights))
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
	// more than once, so only add c/d when not already present. The members of
	// `d` are `t` and `e` only; see expandVirtualRights for why `x` is not one.
	var all strings.Builder
	all.WriteString(string(data.RequiredRights))
	for i := range data.OptionalRights {
		all.WriteString(string(data.OptionalRights[i]))
	}
	allRights := all.String()
	if strings.ContainsRune(allRights, 'k') && !strings.ContainsRune(allRights, 'c') {
		enc.SP().String("c")
	}
	if strings.ContainsAny(allRights, "te") && !strings.ContainsRune(allRights, 'd') {
		enc.SP().String("d")
	}

	return enc.CRLF()
}

func (c *Conn) writeMyRights(data *imap.MyRightsData) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("MYRIGHTS").SP().
		Mailbox(data.Mailbox).SP().
		String(formatRightsWithCompat(data.Rights))

	return enc.CRLF()
}

// For backwards compatibility, keep the old SETACL format helper
func formatRights(rm imap.RightModification, rs imap.RightSet) string {
	return internal.FormatRights(rm, rs)
}

// expandVirtualRights maps the obsolete RFC 2086 rights a client may still name
// (RFC 4314 §2.1.1) onto the rights this server actually has: `c` becomes `k`,
// and `d` becomes `t` `e`.
//
// §2.1.1 leaves the exact composition of `d` to the server: it is "the union"
// of the delete-ish rights the server implements, and mailbox deletion (`x`)
// was split out from message deletion precisely so that a server could grant
// one without the other. Dovecot and Cyrus both read `d` as `e`+`t` and keep
// `x` separate, so a client written against either expects "SETACL ... d" to
// delegate deleting and expunging MESSAGES and not the mailbox itself. Folding
// `x` in would silently hand out a right the client never asked for, and a
// backend that cannot grant `x` would have to refuse the whole request over a
// letter the client never typed. `x` therefore reaches the backend only when
// the client names it.
//
// formatRightsWithCompat and writeListRights are the inverse and must agree:
// they append `d` when `t` or `e` is held (or grantable), never for `x` alone.
func expandVirtualRights(rs imap.RightSet) imap.RightSet {
	res := make(imap.RightSet, 0, len(rs))
	hasC := false
	hasD := false
	for _, r := range rs {
		if r == imap.RightCreate {
			hasC = true
		} else if r == imap.RightDelete {
			hasD = true
		} else {
			res = append(res, r)
		}
	}
	if hasC {
		if !containsRight(res, imap.RightCreateChild) {
			res = append(res, imap.RightCreateChild)
		}
	}
	if hasD {
		for _, dr := range []imap.Right{imap.RightDeleteMsg, imap.RightExpunge} {
			if !containsRight(res, dr) {
				res = append(res, dr)
			}
		}
	}
	return res
}

func containsRight(rs imap.RightSet, r imap.Right) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// formatRightsWithCompat renders a right set for GETACL/MYRIGHTS with the
// obsolete virtual rights appended (RFC 4314 §2.1.1): `c` when `k` is held, `d`
// when `t` or `e` is held. `x` alone does not imply `d`; it is not a member of
// the virtual right here (see expandVirtualRights).
func formatRightsWithCompat(rs imap.RightSet) string {
	hasK := false
	hasTE := false
	for _, r := range rs {
		if r == imap.RightCreateChild {
			hasK = true
		}
		if r == imap.RightDeleteMsg || r == imap.RightExpunge {
			hasTE = true
		}
	}

	s := string(rs)
	if hasK && !strings.ContainsRune(s, 'c') {
		s += "c"
	}
	if hasTE && !strings.ContainsRune(s, 'd') {
		s += "d"
	}
	return s
}
