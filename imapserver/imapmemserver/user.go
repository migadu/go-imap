package imapmemserver

import (
	"context"
	"crypto/subtle"
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

const mailboxDelim rune = '/'

type User struct {
	username, password string

	// notify fans mailbox and message change events out to the NOTIFY
	// (RFC 5465) watchers of this user's sessions.
	notify notifyRegistry

	mutex           sync.Mutex
	mailboxes       map[string]*Mailbox
	prevUidValidity uint32
	serverMetadata  map[string]*[]byte
}

func NewUser(username, password string) *User {
	return &User{
		username:       username,
		password:       password,
		mailboxes:      make(map[string]*Mailbox),
		serverMetadata: make(map[string]*[]byte),
	}
}

func (u *User) Login(ctx context.Context, username, password string) error {
	// Compare both username and password without short-circuiting, so response
	// timing does not distinguish "unknown user" from "wrong password" (username
	// enumeration). Both use constant-time comparison.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(u.username))
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(u.password))
	if userOK != 1 || passOK != 1 {
		return imapserver.ErrAuthFailed
	}
	return nil
}

func (u *User) mailboxLocked(name string) (*Mailbox, error) {
	mbox := u.mailboxes[name]
	if mbox == nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	return mbox, nil
}

// mailboxIfExists returns the mailbox with this name, or nil.
func (u *User) mailboxIfExists(name string) *Mailbox {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	return u.mailboxes[name]
}

func (u *User) mailbox(name string) (*Mailbox, error) {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	return u.mailboxLocked(name)
}

func (u *User) Status(ctx context.Context, name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	mbox, err := u.mailbox(name)
	if err != nil {
		return nil, err
	}
	return mbox.StatusData(options), nil
}

func (u *User) List(ctx context.Context, w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	u.mutex.Lock()
	defer u.mutex.Unlock()

	// TODO: fail if ref doesn't exist

	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{
			Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect},
			Delim: mailboxDelim,
		})
	}

	var l []imap.ListData
	for name, mbox := range u.mailboxes {
		match := false
		for _, pattern := range patterns {
			match = imapserver.MatchList(name, mailboxDelim, ref, pattern)
			if match {
				break
			}
		}
		if !match {
			continue
		}

		data := mbox.list(options)
		if data != nil {
			l = append(l, *data)
		}
	}

	sort.Slice(l, func(i, j int) bool {
		return l[i].Mailbox < l[j].Mailbox
	})

	// Release lock before I/O operations to avoid blocking
	u.mutex.Unlock()
	defer u.mutex.Lock() // Re-lock so the outer defer u.mutex.Unlock() is safe

	for _, data := range l {
		if err := w.WriteList(&data); err != nil {
			return err
		}
	}

	return nil
}

func (u *User) Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	mbox, err := u.mailbox(mailbox)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "No such mailbox",
		}
	}
	return mbox.appendLiteral(r, options)
}

func (u *User) Create(ctx context.Context, name string, options *imap.CreateOptions) error {
	return u.create(name, nil)
}

// create is Create with the session that requested it, so that the resulting
// notification can be withheld from it (RFC 5465 section 5). source may be nil
// when the change does not originate from a session.
func (u *User) create(name string, source *UserSession) error {
	u.mutex.Lock()
	defer u.mutex.Unlock()

	name = strings.TrimRight(name, string(mailboxDelim))

	if u.mailboxes[name] != nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	}

	// UIDVALIDITY must change if a mailbox is deleted and re-created with the
	// same name.
	u.prevUidValidity++
	mbox := NewMailbox(name, u.prevUidValidity)
	mbox.notify = u.notify.broadcast

	// Initialize ACL with full rights for the owner
	mbox.acl[imap.RightsIdentifier(u.username)] = imap.RightSetAll

	u.mailboxes[name] = mbox
	u.notify.broadcast(memNotifyEvent{kind: evMailboxCreated, mailbox: name, source: source})
	u.notifyParentLocked(name, source)
	return nil
}

// notifyParentLocked reports the direct parent of a created or deleted mailbox,
// which RFC 5465 section 5.4 also counts as affected ("the mailbox itself and
// its direct parent (whether it is an existing mailbox or not)").
func (u *User) notifyParentLocked(name string, source *UserSession) {
	i := strings.LastIndexByte(name, byte(mailboxDelim))
	if i <= 0 {
		return
	}
	u.notify.broadcast(memNotifyEvent{kind: evMailboxParent, mailbox: name[:i], source: source})
}

// hasChildren reports whether any mailbox is subordinate to name.
func (u *User) hasChildren(name string) bool {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	prefix := name + string(mailboxDelim)
	for other := range u.mailboxes {
		if strings.HasPrefix(other, prefix) {
			return true
		}
	}
	return false
}

func (u *User) Delete(ctx context.Context, name string) error {
	return u.delete(name, nil)
}

// delete is Delete with the originating session (see create).
func (u *User) delete(name string, source *UserSession) error {
	u.mutex.Lock()
	defer u.mutex.Unlock()

	if _, err := u.mailboxLocked(name); err != nil {
		return err
	}

	delete(u.mailboxes, name)
	u.notify.broadcast(memNotifyEvent{kind: evMailboxDeleted, mailbox: name, source: source})
	u.notifyParentLocked(name, source)
	return nil
}

func (u *User) Rename(ctx context.Context, w *imapserver.RenameWriter, oldName, newName string, options *imap.RenameOptions) error {
	return u.rename(w, oldName, newName, nil)
}

// rename is Rename with the originating session (see create).
func (u *User) rename(w *imapserver.RenameWriter, oldName, newName string, source *UserSession) error {
	u.mutex.Lock()

	newName = strings.TrimRight(newName, string(mailboxDelim))

	mbox, err := u.mailboxLocked(oldName)
	if err != nil {
		u.mutex.Unlock()
		return err
	}

	if u.mailboxes[newName] != nil {
		u.mutex.Unlock()
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeAlreadyExists,
			Text: "Mailbox already exists",
		}
	}

	mbox.rename(newName)
	u.mailboxes[newName] = mbox
	delete(u.mailboxes, oldName)
	u.mutex.Unlock()

	u.notify.broadcast(memNotifyEvent{kind: evMailboxRenamed, mailbox: newName, oldName: oldName, source: source})

	// RFC 9051 §6.3.6: announce the new name with the OLDNAME data item.
	return w.WriteOldName(&imap.ListData{
		Mailbox: newName,
		Delim:   mailboxDelim,
		OldName: oldName,
	})
}

func (u *User) Subscribe(ctx context.Context, name string) error {
	return u.setSubscribed(name, true, nil)
}

func (u *User) Unsubscribe(ctx context.Context, name string) error {
	return u.setSubscribed(name, false, nil)
}

// setSubscribed is Subscribe/Unsubscribe with the originating session (see
// create).
func (u *User) setSubscribed(name string, subscribed bool, source *UserSession) error {
	mbox, err := u.mailbox(name)
	if err != nil {
		return err
	}
	mbox.SetSubscribed(subscribed)
	u.notify.broadcast(memNotifyEvent{kind: evSubscriptionChange, mailbox: name, source: source})
	return nil
}

func (u *User) Namespace(ctx context.Context) (*imap.NamespaceData, error) {
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Delim: mailboxDelim}},
	}, nil
}
