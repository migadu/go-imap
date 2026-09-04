package imapserver

import (
	"context"

	"github.com/emersion/go-imap/v2"
)

// SessionACL is an IMAP session which supports the ACL extension (RFC 4314).
//
// This extension allows clients to manage access control lists for mailboxes,
// enabling shared mailbox functionality with fine-grained permissions.
type SessionACL interface {
	Session

	// GetACL retrieves the access control list for a mailbox.
	// Returns the mailbox name and list of ACL entries.
	//
	// The user must have either the 'l' (lookup) or 'a' (admin) right on the mailbox.
	GetACL(ctx context.Context, mailbox string) (*imap.GetACLData, error)

	// SetACL sets or modifies the access control list for a mailbox.
	// The modification parameter determines how the rights are applied:
	// - RightModificationReplace: Replace all rights for the identifier
	// - RightModificationAdd: Add the specified rights to existing rights
	// - RightModificationRemove: Remove the specified rights from existing rights
	//
	// To remove all rights for an identifier, use RightModificationReplace with an empty rights set.
	//
	// The user must have the 'a' (admin) right on the mailbox.
	//
	// identifier: User email, group name, or special identifier ("anyone", "authenticated")
	// modification: How to apply the rights (replace, add, or remove)
	// rights: Rights to grant/add/remove
	SetACL(ctx context.Context, mailbox string, identifier imap.RightsIdentifier, modification imap.RightModification, rights imap.RightSet) error

	// DeleteACL removes the access control list entry for an identifier.
	// This is equivalent to SetACL with RightModificationReplace and empty rights.
	//
	// The user must have the 'a' (admin) right on the mailbox.
	DeleteACL(ctx context.Context, mailbox string, identifier imap.RightsIdentifier) error

	// ListRights lists the rights that can be granted to an identifier on a mailbox.
	// Returns required rights (always present) and groups of optional rights (may be granted).
	//
	// The user must have the 'a' (admin) right on the mailbox.
	ListRights(ctx context.Context, mailbox string, identifier imap.RightsIdentifier) (*imap.ListRightsData, error)

	// MyRights returns the rights the current user has on a mailbox.
	// This command does not require any special permissions - any user can check their own rights.
	MyRights(ctx context.Context, mailbox string) (*imap.MyRightsData, error)
}

// SessionACLVirtualRights is an optional extension of SessionACL that lets a
// backend define the membership of the two virtual rights RFC 4314 §2.1.1 keeps
// alive for RFC 2086 clients: `c` (create) and `d` (delete).
//
// The section describes two server families and leaves the choice to the
// server: those whose RFC 2086 `c` controlled DELETE read `c` as `k`+`x` and
// `d` as `e`+`t`, those whose `d` did read `c` as `k` and `d` as `e`+`t`+`x`.
// A backend that implements none of this gets the first family
// (DefaultVirtualCreate, DefaultVirtualDelete), which is Dovecot's fixed
// reading and Cyrus' default. A backend for which `x` is not something a grant
// can carry at all may leave it out of both, so that a client's "SETACL ... c"
// is not expanded into a request the backend then has to refuse over a letter
// the client never typed.
//
// The declaration is the ONE source for everything the server does with the
// virtual rights: the expansion of an incoming `c`/`d` in SETACL, the `c`/`d`
// appended to GETACL and MYRIGHTS, and the `c`/`d` groups added to LISTRIGHTS.
// Reading all three from one place is what keeps them from disagreeing, which
// is why this is a single method returning both sets rather than one per
// right. The two sets are normalized before use: the virtual letters
// themselves and duplicates are dropped, and a right named in both stays in
// `create` only, since in both RFC families a right is a member of at most one
// virtual right.
//
// The RIGHTS= capability is deliberately NOT derived from this. RFC 4314 §6's
// formal syntax fixes it ("new-rights ... MUST include t, e, x, and k"), so it
// names the rights the server implements, not the ones a grant may carry.
type SessionACLVirtualRights interface {
	SessionACL

	// VirtualRights returns the members of the virtual `c` (create) and `d`
	// (delete) rights, in that order. Either may be empty, in which case the
	// server does not have that virtual right: a client naming it in SETACL is
	// refused with BAD (RFC 4314 §3.1, unrecognized right), and it is never
	// appended to GETACL, MYRIGHTS or LISTRIGHTS.
	VirtualRights() (create, delete imap.RightSet)
}

// DefaultVirtualCreate and DefaultVirtualDelete are the members of the virtual
// `c` and `d` rights for a SessionACL that does not implement
// SessionACLVirtualRights: RFC 4314 §2.1.1's first family, `c` = `k`+`x` and
// `d` = `t`+`e`.
var (
	DefaultVirtualCreate = imap.RightSet{imap.RightCreateChild, imap.RightDeleteMbox}
	DefaultVirtualDelete = imap.RightSet{imap.RightDeleteMsg, imap.RightExpunge}
)
