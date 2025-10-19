package imapserver

import "github.com/emersion/go-imap/v2"

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
	GetACL(mailbox string) (*imap.GetACLData, error)

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
	SetACL(mailbox string, identifier imap.RightsIdentifier, modification imap.RightModification, rights imap.RightSet) error

	// DeleteACL removes the access control list entry for an identifier.
	// This is equivalent to SetACL with RightModificationReplace and empty rights.
	//
	// The user must have the 'a' (admin) right on the mailbox.
	DeleteACL(mailbox string, identifier imap.RightsIdentifier) error

	// ListRights lists the rights that can be granted to an identifier on a mailbox.
	// Returns required rights (always present) and groups of optional rights (may be granted).
	//
	// The user must have the 'a' (admin) right on the mailbox.
	ListRights(mailbox string, identifier imap.RightsIdentifier) (*imap.ListRightsData, error)

	// MyRights returns the rights the current user has on a mailbox.
	// This command does not require any special permissions - any user can check their own rights.
	MyRights(mailbox string) (*imap.MyRightsData, error)
}
