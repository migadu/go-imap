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

var _ imapserver.SessionIMAP4rev2 = (*UserSession)(nil)

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
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()
	sess.mailbox = mbox.NewView()
	return mbox.selectDataLocked(), nil
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

	for _, requestedEntry := range entries {
		for entryName, value := range source {
			if matchesWithDepth(entryName, requestedEntry, options) {
				if options != nil && options.MaxSize != nil && value != nil {
					if uint32(len(*value)) > *options.MaxSize {
						continue
					}
				}
				result[entryName] = value
			}
		}
	}

	return &imap.GetMetadataData{
		Mailbox: mailboxName,
		Entries: result,
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
