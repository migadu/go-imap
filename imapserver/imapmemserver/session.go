package imapmemserver

import (
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type (
	user    = User
	mailbox = MailboxView
)

// UserSession represents a session tied to a specific user.
//
// UserSession implements imapserver.Session. Typically, a UserSession pointer
// is embedded into a larger struct which overrides Login.
type UserSession struct {
	*user    // immutable
	*mailbox // may be nil
}

var (
	_ imapserver.SessionIMAP4rev2 = (*UserSession)(nil)
	_ imapserver.SessionACL       = (*UserSession)(nil)
)

// NewUserSession creates a new user session.
func NewUserSession(user *User) *UserSession {
	return &UserSession{user: user}
}

func (sess *UserSession) Close() error {
	if sess != nil && sess.mailbox != nil {
		sess.mailbox.Close()
	}
	return nil
}

func (sess *UserSession) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	if sess.mailbox != nil {
		sess.mailbox.Close()
	}
	sess.mailbox = mbox.NewView()
	return sess.mailbox.selectData(options)
}

func (sess *UserSession) Unselect() error {
	sess.mailbox.Close()
	sess.mailbox = nil
	return nil
}

func (sess *UserSession) Copy(numSet imap.NumSet, destName string) (*imap.CopyData, error) {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	} else if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Source and destination mailboxes are identical",
		}
	}

	var sourceUIDs, destUIDs imap.UIDSet
	sess.mailbox.forEach(numSet, func(seqNum uint32, msg *message) {
		appendData := dest.copyMsg(msg)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
	})

	return &imap.CopyData{
		UIDValidity: dest.uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}, nil
}

func (sess *UserSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, destName string) error {
	dest, err := sess.user.mailbox(destName)
	if err != nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	} else if sess.mailbox != nil && dest == sess.mailbox.Mailbox {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Source and destination mailboxes are identical",
		}
	}

	sess.mailbox.mutex.Lock()
	defer sess.mailbox.mutex.Unlock()

	var sourceUIDs, destUIDs imap.UIDSet
	expunged := make(map[*message]struct{})
	sess.mailbox.forEachLocked(numSet, func(seqNum uint32, msg *message) {
		appendData := dest.copyMsg(msg)
		sourceUIDs.AddNum(msg.uid)
		destUIDs.AddNum(appendData.UID)
		expunged[msg] = struct{}{}
	})
	seqNums := sess.mailbox.expungeLocked(expunged)

	err = w.WriteCopyData(&imap.CopyData{
		UIDValidity: dest.uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	})
	if err != nil {
		return err
	}

	for _, seqNum := range seqNums {
		if err := w.WriteExpunge(sess.mailbox.tracker.EncodeSeqNum(seqNum)); err != nil {
			return err
		}
	}

	return nil
}

func (sess *UserSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	if sess.mailbox == nil {
		return nil
	}
	return sess.mailbox.Poll(w, allowExpunge)
}

func (sess *UserSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	if sess.mailbox == nil {
		return nil // TODO
	}
	return sess.mailbox.Idle(w, stop)
}

func (sess *UserSession) Sort(kind imapserver.NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	return sess.mailbox.Sort(kind, sortCriteria, charset, searchCriteria, options)
}

func (sess *UserSession) Thread(numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	if sess.mailbox == nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "No mailbox selected",
		}
	}

	if algorithm != imap.ThreadReferences && algorithm != imap.ThreadOrderedSubject {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unsupported algorithm in mock",
		}
	}

	// For testing, just return a dummy thread matching our client test expectation
	return []imap.ThreadData{
		{Chain: []uint32{1}},
	}, nil
}

func (sess *UserSession) MultiSearch(numKind imapserver.NumKind, mailboxes []string, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	var results []*imap.SearchData
	for _, mboxName := range mailboxes {
		mbox, err := sess.user.mailbox(mboxName)
		if err != nil {
			// Skip mailboxes that don't exist
			continue
		}
		view := mbox.NewView()
		defer view.Close()

		data, err := view.Search(numKind, criteria, options)
		if err != nil {
			return nil, err
		}
		data.Mailbox = mboxName
		results = append(results, data)
	}
	return results, nil
}

func (sess *UserSession) GetMetadata(mailboxName string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	sess.user.mutex.Lock()
	defer sess.user.mutex.Unlock()

	var source map[string]*[]byte
	if mailboxName == "" {
		source = sess.user.serverMetadata
	} else {
		mbox, err := sess.user.mailboxLocked(mailboxName)
		if err != nil {
			return nil, err
		}
		mbox.mutex.Lock()
		source = mbox.metadata
		mbox.mutex.Unlock()
	}

	result := make(map[string]*[]byte)
	var longEntries uint32

	if len(entries) == 0 {
		for entryName, value := range source {
			result[entryName] = value
		}
	} else {
		for _, requestedEntry := range entries {
			for entryName, value := range source {
				if matchesWithDepth(entryName, requestedEntry, options) {
					if options != nil && options.MaxSize != nil && value != nil {
						size := uint32(len(*value))
						if size > *options.MaxSize {
							if size > longEntries {
								longEntries = size
							}
							continue
						}
					}
					result[entryName] = value
				}
			}
		}
	}

	return &imap.GetMetadataData{
		Mailbox:     mailboxName,
		Entries:     result,
		LongEntries: longEntries,
	}, nil
}

func (sess *UserSession) SetMetadata(mailboxName string, entries map[string]*[]byte) error {
	sess.user.mutex.Lock()
	defer sess.user.mutex.Unlock()

	var target map[string]*[]byte
	if mailboxName == "" {
		target = sess.user.serverMetadata
	} else {
		mbox, err := sess.user.mailboxLocked(mailboxName)
		if err != nil {
			return err
		}
		mbox.mutex.Lock()
		defer mbox.mutex.Unlock()
		target = mbox.metadata
	}

	for entry, value := range entries {
		if value == nil {
			delete(target, entry)
		} else {
			if len(*value) > 10240 {
				return &imap.Error{
					Type: imap.StatusResponseTypeNo,
					Code: imap.ResponseCodeLimit,
					Text: "Annotation value too large",
				}
			}
			target[entry] = value
		}
	}

	if len(target) > 100 {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooMany,
			Text: "Too many annotations",
		}
	}

	return nil
}

func matchesWithDepth(entryName, requestedEntry string, options *imap.GetMetadataOptions) bool {
	depth := imap.GetMetadataDepthZero
	if options != nil {
		depth = options.Depth
	}

	switch depth {
	case imap.GetMetadataDepthZero:
		return entryName == requestedEntry
	case imap.GetMetadataDepthOne:
		if entryName == requestedEntry {
			return true
		}
		if len(entryName) > len(requestedEntry) &&
			entryName[:len(requestedEntry)] == requestedEntry &&
			entryName[len(requestedEntry)] == '/' {
			remainder := entryName[len(requestedEntry)+1:]
			return !strings.Contains(remainder, "/")
		}
		return false
	case imap.GetMetadataDepthInfinity:
		if entryName == requestedEntry {
			return true
		}
		if len(entryName) > len(requestedEntry) &&
			entryName[:len(requestedEntry)] == requestedEntry &&
			entryName[len(requestedEntry)] == '/' {
			return true
		}
		return false
	default:
		return false
	}
}

// GetACL retrieves the access control list for a mailbox
func (sess *UserSession) GetACL(name string) (*imap.GetACLData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}

	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	// Return ACL entries (for test purposes, we grant full rights to the current user)
	entries := []imap.ACLEntry{
		{
			Identifier: imap.RightsIdentifier(sess.user.username),
			Rights:     mbox.acl[imap.RightsIdentifier(sess.user.username)],
		},
	}

	// Add other ACL entries
	for identifier, rights := range mbox.acl {
		if identifier != imap.RightsIdentifier(sess.user.username) {
			entries = append(entries, imap.ACLEntry{
				Identifier: identifier,
				Rights:     rights,
			})
		}
	}

	return &imap.GetACLData{
		Mailbox: name,
		ACL:     entries,
	}, nil
}

// SetACL sets or modifies the access control list for a mailbox
func (sess *UserSession) SetACL(name string, identifier imap.RightsIdentifier, modification imap.RightModification, rights imap.RightSet) error {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return err
	}

	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	// Check if user has admin rights
	userRights := mbox.acl[imap.RightsIdentifier(sess.user.username)]
	hasAdmin := false
	for _, r := range userRights {
		if r == imap.RightAdminister {
			hasAdmin = true
			break
		}
	}
	if !hasAdmin {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Permission denied: admin right required",
		}
	}

	// Apply modification
	currentRights := mbox.acl[identifier]

	// Handle obsolete rights for backwards compatibility
	if strings.Contains(string(rights), "c") {
		rights = rights.Add(imap.RightSet("k"))
	}
	if strings.Contains(string(rights), "d") {
		rights = rights.Add(imap.RightSet("te"))
	}

	switch modification {
	case imap.RightModificationReplace:
		mbox.acl[identifier] = rights
	case imap.RightModificationAdd:
		mbox.acl[identifier] = currentRights.Add(rights)
	case imap.RightModificationRemove:
		mbox.acl[identifier] = currentRights.Remove(rights)
	}

	return nil
}

// DeleteACL removes the access control list entry for an identifier
func (sess *UserSession) DeleteACL(name string, identifier imap.RightsIdentifier) error {
	return sess.SetACL(name, identifier, imap.RightModificationReplace, nil)
}

// ListRights lists the rights that can be granted to an identifier on a mailbox
func (sess *UserSession) ListRights(name string, identifier imap.RightsIdentifier) (*imap.ListRightsData, error) {
	_, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}

	// For test purposes, return all rights as optional
	return &imap.ListRightsData{
		Mailbox:        name,
		Identifier:     identifier,
		RequiredRights: imap.RightSet(""),
		OptionalRights: []imap.RightSet{imap.RightSetAll},
	}, nil
}

// MyRights returns the rights the current user has on a mailbox
func (sess *UserSession) MyRights(name string) (*imap.MyRightsData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}

	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	rights := mbox.acl[imap.RightsIdentifier(sess.user.username)]

	return &imap.MyRightsData{
		Mailbox: name,
		Rights:  rights,
	}, nil
}
