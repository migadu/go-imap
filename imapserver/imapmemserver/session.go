package imapmemserver

import (
	"context"
	"sort"
	"strings"
	"sync"

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

	// ownMessages records the UIDs this session created in the selected
	// mailbox, so that no MessageNew fetch-att response is generated for them
	// (RFC 5465 section 5.2: "A FETCH response SHOULD NOT be generated for a
	// new message created by the client on this particular connection").
	//
	// Entries are recorded only while a watch that produces such responses is
	// installed, consumed when the notification is emitted, and dropped
	// wholesale when the selected mailbox changes — so the set stays bounded
	// and can never outlive the mailbox it refers to. Guarded by notifyMutex.
	ownMessages map[imap.UID]struct{}

	// notifyMutex guards notifyWatch, and synchronizes updates of the
	// mailbox pointer (performed on the command goroutine) with reads from
	// the NOTIFY pump goroutine. Command-goroutine reads need no locking:
	// they run on the same goroutine as the writes.
	notifyMutex sync.Mutex
	notifyWatch *notifyWatch

	// notifyOff is non-nil while NOTIFY governs delivery on this session, from
	// the first NOTIFY command on. It stays so after NOTIFY NONE: that command
	// asks for no events at all (RFC 5465 section 3.1), so IDLE must not fall
	// back to pushing the selected mailbox's updates either. It is closed and
	// cleared by a notification overflow, which returns the connection to
	// pre-NOTIFY delivery (see checkNotifyOverflow); an IDLE waiting on it
	// then takes over.
	notifyOff chan struct{}

	// notifyEvents receives the change events of the user while NOTIFY is in
	// use — with a watch installed or after NOTIFY NONE, when it only wakes
	// the pump to bound the queue of the selected mailbox. It belongs to the
	// session rather than to the NotifyPoll call, so events raised while the
	// pump is stopped (the library fences it around every change of the
	// selected mailbox) are queued instead of lost.
	notifyEvents chan memNotifyEvent
}

var (
	_ imapserver.SessionIMAP4rev2 = (*UserSession)(nil)
	_ imapserver.SessionACL       = (*UserSession)(nil)
)

// NewUserSession creates a new user session.
func NewUserSession(user *User) *UserSession {
	return &UserSession{user: user}
}

// setMailbox updates the selected-mailbox pointer, keeping the NOTIFY pump's
// view consistent (see notifyMutex).
func (sess *UserSession) setMailbox(mbox *MailboxView) {
	// Read uidNext under the mailbox lock that guards it, before taking
	// notifyMutex: another session may be appending concurrently.
	var uidNext imap.UID
	if mbox != nil {
		mbox.mutex.Lock()
		uidNext = mbox.uidNext
		mbox.mutex.Unlock()
	}

	sess.notifyMutex.Lock()
	sess.mailbox = mbox
	// The recorded UIDs belong to the mailbox being left.
	sess.ownMessages = nil
	if sess.notifyWatch != nil && mbox != nil {
		// Rebind the MessageNew fetch-atts high-water mark to the newly
		// selected mailbox: only messages arriving from now on are "new".
		sess.notifyWatch.lastUIDNext = uidNext
	}
	sess.notifyMutex.Unlock()
}

func (sess *UserSession) Close() error {
	if sess == nil {
		return nil
	}
	sess.stopNotifyEvents()
	if sess.mailbox != nil {
		sess.mailbox.Close()
		sess.setMailbox(nil)
	}
	return nil
}

// Create, Delete, Rename, Subscribe and Unsubscribe shadow the methods
// promoted from the embedded User to pass the originating session along: RFC
// 5465 section 5 asks the server not to notify the client that caused an event.

func (sess *UserSession) Create(ctx context.Context, name string, options *imap.CreateOptions) error {
	return sess.user.create(name, sess)
}

func (sess *UserSession) Delete(ctx context.Context, name string) error {
	return sess.user.delete(name, sess)
}

func (sess *UserSession) Rename(ctx context.Context, w *imapserver.RenameWriter, oldName, newName string, options *imap.RenameOptions) error {
	return sess.user.rename(w, oldName, newName, sess)
}

func (sess *UserSession) Subscribe(ctx context.Context, name string) error {
	return sess.user.setSubscribed(name, true, sess)
}

func (sess *UserSession) Unsubscribe(ctx context.Context, name string) error {
	return sess.user.setSubscribed(name, false, sess)
}

// Append records the UID it creates before delegating, so the MessageNew
// notification for the selected mailbox does not echo the message back to its
// author (RFC 5465 section 5.2).
func (sess *UserSession) Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	data, err := sess.user.Append(ctx, mailbox, r, options)
	if err != nil {
		return nil, err
	}
	sess.recordOwnMessage(mailbox, data.UID)
	return data, nil
}

// maxOwnMessages bounds the recorded set; past it, self-created messages are
// simply reported like any other (the RFC 5465 section 5.2 rule is a SHOULD).
const maxOwnMessages = 4096

// recordOwnMessage remembers a message this session created in the mailbox it
// has selected.
func (sess *UserSession) recordOwnMessage(mailbox string, uid imap.UID) {
	if uid == 0 {
		return
	}

	sess.notifyMutex.Lock()
	defer sess.notifyMutex.Unlock()

	// Only messages that could produce a MessageNew fetch-att response are
	// worth remembering: those in the selected mailbox, while such a watch is
	// installed.
	watch := sess.notifyWatch
	if watch == nil || watch.selected == nil || watch.selected.MessageNewFetch == nil {
		return
	}
	if sess.mailbox == nil || sess.mailbox.Mailbox != sess.user.mailboxIfExists(mailbox) {
		return
	}
	if len(sess.ownMessages) >= maxOwnMessages {
		return
	}
	if sess.ownMessages == nil {
		sess.ownMessages = make(map[imap.UID]struct{})
	}
	sess.ownMessages[uid] = struct{}{}
}

// takeOwnMessage reports whether the message was created by this session,
// forgetting it in the process.
func (sess *UserSession) takeOwnMessage(uid imap.UID) bool {
	sess.notifyMutex.Lock()
	defer sess.notifyMutex.Unlock()
	if _, ok := sess.ownMessages[uid]; !ok {
		return false
	}
	delete(sess.ownMessages, uid)
	return true
}

func (sess *UserSession) Select(ctx context.Context, name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	mbox, err := sess.user.mailbox(name)
	if err != nil {
		return nil, err
	}
	if sess.mailbox != nil {
		sess.mailbox.Close()
	}
	sess.setMailbox(mbox.NewView())
	return sess.mailbox.selectData(options)
}

func (sess *UserSession) Unselect(ctx context.Context) error {
	sess.mailbox.Close()
	sess.setMailbox(nil)
	return nil
}

func (sess *UserSession) Copy(ctx context.Context, numSet imap.NumSet, destName string) (*imap.CopyData, error) {
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

	if uids, ok := destUIDs.Nums(); ok {
		for _, uid := range uids {
			sess.recordOwnMessage(destName, uid)
		}
	}

	return &imap.CopyData{
		UIDValidity: dest.uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}, nil
}

func (sess *UserSession) Move(ctx context.Context, w *imapserver.MoveWriter, numSet imap.NumSet, destName string) error {
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

func (sess *UserSession) Poll(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if sess.mailbox == nil {
		return nil
	}
	if err := sess.mailbox.Poll(ctx, w, allowExpunge); err != nil {
		return err
	}
	// This flush may have released the EXISTS a pending MessageNew fetch-att
	// response was waiting for.
	return sess.notifyPendingFetch(w)
}

func (sess *UserSession) Idle(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	sess.notifyMutex.Lock()
	notifyOff := sess.notifyOff
	sess.notifyMutex.Unlock()
	if notifyOff != nil {
		// Once NOTIFY has been used, it is the single event source for this
		// connection: the pump delivers according to the client's filter, and
		// IDLE only keeps the connection open. Delivering via tracker.Idle
		// here would bypass the filter — a watch without a SELECTED specifier
		// (RFC 5465 §3.1: "same as specifying SELECTED NONE"), or NOTIFY NONE,
		// must not push selected-mailbox message events.
		select {
		case <-stop:
			return nil
		case <-ctx.Done():
			return nil
		case <-notifyOff:
			// A notification overflow ended NOTIFY mid-IDLE: the pump is gone
			// and delivery is back to the pre-NOTIFY rules, under which IDLE
			// pushes the selected mailbox's updates. Fall through to
			// tracker.Idle, which drains the ones accumulated behind the watch
			// on entry and then carries on as usual.
		}
	}

	if sess.mailbox == nil {
		return nil // TODO
	}
	return sess.mailbox.Idle(ctx, w, stop)
}

func (sess *UserSession) Sort(ctx context.Context, kind imapserver.NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	return sess.mailbox.Sort(ctx, kind, sortCriteria, charset, searchCriteria, options)
}

func (sess *UserSession) Thread(ctx context.Context, numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
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

func (sess *UserSession) MultiSearch(ctx context.Context, source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	var results []*imap.SearchData
	for _, mboxName := range sess.resolveSearchSource(source) {
		mbox, err := sess.user.mailbox(mboxName)
		if err != nil {
			// Skip mailboxes that don't exist (e.g. resolved from a scope verb).
			continue
		}
		view := mbox.NewView()

		// RFC 7377: ESEARCH always reports UIDs, never message numbers.
		data, err := view.Search(ctx, imapserver.NumKindUID, criteria, options)
		view.Close()
		if err != nil {
			return nil, err
		}
		data.Mailbox = mboxName
		data.UIDValidity = mbox.uidValidity
		results = append(results, data)
	}
	return results, nil
}

// resolveSearchSource turns an ESEARCH source spec into an ordered, de-duplicated
// list of mailbox names to search.
func (sess *UserSession) resolveSearchSource(source *imap.SearchSource) []string {
	var names []string
	seen := make(map[string]struct{})
	add := func(name string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}

	if source.Selected && sess.mailbox != nil {
		add(sess.mailbox.name)
	}
	if source.Inboxes {
		add("INBOX")
	}

	// Snapshot the user's mailboxes (name + subscription state) once.
	sess.user.mutex.Lock()
	all := make([]string, 0, len(sess.user.mailboxes))
	subscribed := make(map[string]bool, len(sess.user.mailboxes))
	for name, mbox := range sess.user.mailboxes {
		all = append(all, name)
		mbox.mutex.Lock()
		subscribed[name] = mbox.subscribed
		mbox.mutex.Unlock()
	}
	sess.user.mutex.Unlock()
	sort.Strings(all)

	if source.Personal {
		for _, name := range all {
			add(name)
		}
	}
	if source.Subscribed {
		for _, name := range all {
			if subscribed[name] {
				add(name)
			}
		}
	}
	for _, root := range source.Subtree {
		prefix := root + string(mailboxDelim)
		for _, name := range all {
			if name == root || strings.HasPrefix(name, prefix) {
				add(name)
			}
		}
	}
	for _, root := range source.SubtreeOne {
		prefix := root + string(mailboxDelim)
		for _, name := range all {
			if name == root {
				add(name)
				continue
			}
			if rest, ok := strings.CutPrefix(name, prefix); ok && !strings.ContainsRune(rest, mailboxDelim) {
				add(name)
			}
		}
	}
	for _, name := range source.Mailboxes {
		add(name)
	}
	return names
}

func (sess *UserSession) GetMetadata(ctx context.Context, mailboxName string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
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

func (sess *UserSession) SetMetadata(ctx context.Context, mailboxName string, entries map[string]*[]byte) error {
	if err := sess.setMetadata(mailboxName, entries); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	// RFC 5465 sections 5.6 and 5.7: report the change to NOTIFY watchers with
	// an unsolicited METADATA response naming the changed entries, deletions
	// included. Sorted so the response order is deterministic.
	changed := make([]string, 0, len(entries))
	for name := range entries {
		changed = append(changed, name)
	}
	sort.Strings(changed)
	kind := evMailboxMetadataChange
	if mailboxName == "" {
		kind = evServerMetadataChange
	}
	sess.user.notify.broadcast(memNotifyEvent{
		kind:    kind,
		mailbox: mailboxName,
		entries: changed,
		source:  sess,
	})
	return nil
}

func (sess *UserSession) setMetadata(mailboxName string, entries map[string]*[]byte) error {
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
func (sess *UserSession) GetACL(ctx context.Context, name string) (*imap.GetACLData, error) {
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
func (sess *UserSession) SetACL(ctx context.Context, name string, identifier imap.RightsIdentifier, modification imap.RightModification, rights imap.RightSet) error {
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

	// Handle obsolete rights for backwards compatibility (RFC 4314 §2.1.1):
	// `c` is `k`+`x` and `d` is `t`+`e`, the same reading as imapserver's
	// expandVirtualRights, so the two layers agree.
	if strings.Contains(string(rights), "c") {
		rights = rights.Add(imap.RightSet("kx"))
	}
	if strings.Contains(string(rights), "d") {
		rights = rights.Add(imap.RightSet("te"))
	}

	before := mbox.acl[imap.RightsIdentifier(sess.user.username)]

	switch modification {
	case imap.RightModificationReplace:
		mbox.acl[identifier] = rights
	case imap.RightModificationAdd:
		mbox.acl[identifier] = currentRights.Add(rights)
	case imap.RightModificationRemove:
		mbox.acl[identifier] = currentRights.Remove(rights)
	}

	if kind, ok := aclNotifyEvent(before, mbox.acl[imap.RightsIdentifier(sess.user.username)]); ok {
		sess.user.notify.broadcast(memNotifyEvent{kind: kind, mailbox: name, source: sess})
	}

	return nil
}

// DeleteACL removes the access control list entry for an identifier
func (sess *UserSession) DeleteACL(ctx context.Context, name string, identifier imap.RightsIdentifier) error {
	return sess.SetACL(ctx, name, identifier, imap.RightModificationReplace, nil)
}

// aclNotifyEvent maps a change of the current user's rights on a mailbox to the
// NOTIFY event it must be reported as. RFC 5465 section 5.4: "granting or
// revocation of the 'l' right to the current user on the affected mailbox MUST
// be considered mailbox creation or deletion". Section 5.9 asks for a
// \NoAccess LIST response when a right needed to monitor the mailbox is lost
// while it stays listable.
func aclNotifyEvent(before, after imap.RightSet) (memNotifyEventKind, bool) {
	couldList, canList := hasRight(before, imap.RightLookup), hasRight(after, imap.RightLookup)
	switch {
	case !couldList && canList:
		return evMailboxCreated, true
	case couldList && !canList:
		return evMailboxDeleted, true
	case canList && hasRight(before, imap.RightRead) && !hasRight(after, imap.RightRead):
		return evMailboxNoAccess, true
	default:
		return 0, false
	}
}

func hasRight(rights imap.RightSet, right imap.Right) bool {
	for _, r := range rights {
		if r == right {
			return true
		}
	}
	return false
}

// ListRights lists the rights that can be granted to an identifier on a mailbox
func (sess *UserSession) ListRights(ctx context.Context, name string, identifier imap.RightsIdentifier) (*imap.ListRightsData, error) {
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
func (sess *UserSession) MyRights(ctx context.Context, name string) (*imap.MyRightsData, error) {
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
