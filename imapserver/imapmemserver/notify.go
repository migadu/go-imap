package imapmemserver

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// memNotifyEventKind enumerates the internal change events broadcast to
// NOTIFY watchers.
type memNotifyEventKind int

const (
	evMailboxCreated memNotifyEventKind = iota
	evMailboxDeleted
	evMailboxRenamed
	evSubscriptionChange
	evMessageNew
	evMessageExpunge
	evFlagChange
)

// isMessageEvent reports whether the internal event maps to an RFC 5465
// <message-event> (as opposed to a mailbox event).
func (kind memNotifyEventKind) isMessageEvent() bool {
	switch kind {
	case evMessageNew, evMessageExpunge, evFlagChange:
		return true
	default:
		return false
	}
}

// notifyEvent maps the internal event kind to the RFC 5465 event a client
// must have requested to receive it.
func (kind memNotifyEventKind) notifyEvent() imap.NotifyEvent {
	switch kind {
	case evMailboxCreated, evMailboxDeleted, evMailboxRenamed:
		return imap.NotifyEventMailboxName
	case evSubscriptionChange:
		return imap.NotifyEventSubscriptionChange
	case evMessageNew:
		return imap.NotifyEventMessageNew
	case evMessageExpunge:
		return imap.NotifyEventMessageExpunge
	case evFlagChange:
		return imap.NotifyEventFlagChange
	default:
		panic("imapmemserver: unknown notify event kind")
	}
}

type memNotifyEvent struct {
	kind    memNotifyEventKind
	mailbox string // current mailbox name
	oldName string // previous name, for evMailboxRenamed
}

// notifyRegistry fans change events out to the NOTIFY watchers of a user.
// Watcher channels are buffered; events for a watcher that has fallen behind
// are dropped (the in-memory server favors simplicity — a production backend
// would send NOTIFICATIONOVERFLOW instead).
type notifyRegistry struct {
	mutex    sync.Mutex
	watchers map[chan<- memNotifyEvent]struct{}
}

func (r *notifyRegistry) register(ch chan<- memNotifyEvent) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.watchers == nil {
		r.watchers = make(map[chan<- memNotifyEvent]struct{})
	}
	r.watchers[ch] = struct{}{}
}

func (r *notifyRegistry) unregister(ch chan<- memNotifyEvent) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	delete(r.watchers, ch)
}

func (r *notifyRegistry) broadcast(ev memNotifyEvent) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for ch := range r.watchers {
		select {
		case ch <- ev:
		default:
			// Watcher queue full: drop the event.
		}
	}
}

// memNotifySupportedEvents is the set of RFC 5465 events the in-memory
// server supports, advertised in the BADEVENT response code.
var memNotifySupportedEvents = []imap.NotifyEvent{
	imap.NotifyEventMessageNew,
	imap.NotifyEventMessageExpunge,
	imap.NotifyEventFlagChange,
	imap.NotifyEventMailboxName,
	imap.NotifyEventSubscriptionChange,
}

// notifyWatch is the per-session NOTIFY state installed by SetNotify.
type notifyWatch struct {
	options *imap.NotifyOptions

	// selected is the item with the SELECTED/SELECTED-DELAYED specifier, if
	// any.
	selected *imap.NotifyItem

	// lastUIDNext is the UIDNEXT high-water mark of the selected mailbox,
	// used to identify the new messages a MessageNew FETCH response must be
	// generated for.
	lastUIDNext imap.UID
}

// selectedEvents reports whether the selected-mailbox item requests the
// given event.
func (watch *notifyWatch) selectedEvents(event imap.NotifyEvent) bool {
	if watch.selected == nil {
		return false
	}
	for _, ev := range watch.selected.Events {
		if ev == event {
			return true
		}
	}
	return false
}

var _ imapserver.SessionNotify = (*UserSession)(nil)

// SetNotify implements imapserver.SessionNotify.
func (sess *UserSession) SetNotify(ctx context.Context, options *imap.NotifyOptions, w *imapserver.UpdateWriter) error {
	if options == nil {
		sess.notifyMutex.Lock()
		sess.notifyWatch = nil
		sess.notifyMutex.Unlock()
		return nil
	}

	for _, item := range options.Items {
		for _, ev := range item.Events {
			switch ev {
			case imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge,
				imap.NotifyEventFlagChange, imap.NotifyEventMailboxName,
				imap.NotifyEventSubscriptionChange:
				// supported
			default:
				return &imapserver.UnsupportedNotifyEventError{
					Supported: memNotifySupportedEvents,
				}
			}
		}
	}

	watch := &notifyWatch{options: options}
	for i := range options.Items {
		item := &options.Items[i]
		if item.MailboxSpec == imap.NotifyMailboxSpecSelected ||
			item.MailboxSpec == imap.NotifyMailboxSpecSelectedDelayed {
			watch.selected = item
		}
	}

	sess.notifyMutex.Lock()
	selectedName := ""
	if sess.mailbox != nil {
		selectedName = sess.mailbox.name
		watch.lastUIDNext = sess.mailbox.uidNext
	}
	sess.notifyMutex.Unlock()

	// RFC 5465 section 3.1: with the STATUS indicator, send a STATUS
	// response for each non-selected mailbox with message events enabled,
	// before NOTIFY's tagged OK (SetNotify runs synchronously before the
	// tagged response is written).
	if options.Status {
		for _, name := range sess.user.sortedMailboxNames() {
			if name == selectedName {
				continue
			}
			events := sess.notifyEventsForMailbox(watch, name)
			statusOptions := notifyStatusOptions(events, w.CondStoreEnabled())
			if statusOptions == nil {
				continue
			}
			mbox, err := sess.user.mailbox(name)
			if err != nil {
				continue
			}
			if err := w.WriteStatus(mbox.StatusData(statusOptions), statusOptions); err != nil {
				return err
			}
		}
	}

	sess.notifyMutex.Lock()
	sess.notifyWatch = watch
	sess.notifyMutex.Unlock()
	return nil
}

// NotifyPoll implements imapserver.SessionNotify.
func (sess *UserSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	sess.notifyMutex.Lock()
	watch := sess.notifyWatch
	sess.notifyMutex.Unlock()
	if watch == nil {
		return nil
	}

	ch := make(chan memNotifyEvent, 256)
	sess.user.notify.register(ch)
	defer sess.user.notify.unregister(ch)

	for {
		select {
		case <-stop:
			return nil
		case <-ctx.Done():
			return nil
		case ev := <-ch:
			if err := sess.handleNotifyEvent(ev, w); err != nil {
				return err
			}
		}
	}
}

func (sess *UserSession) handleNotifyEvent(ev memNotifyEvent, w *imapserver.UpdateWriter) error {
	sess.notifyMutex.Lock()
	watch := sess.notifyWatch
	mailbox := sess.mailbox
	sess.notifyMutex.Unlock()
	if watch == nil {
		return nil
	}

	selectedName := ""
	if mailbox != nil {
		selectedName = mailbox.name
	}

	if ev.kind.isMessageEvent() && ev.mailbox == selectedName && mailbox != nil {
		return sess.handleSelectedNotifyEvent(ev, watch, mailbox, w)
	}

	events := sess.notifyEventsForMailbox(watch, ev.mailbox)
	if !events[ev.kind.notifyEvent()] {
		return nil
	}

	switch ev.kind {
	case evMailboxCreated, evMailboxDeleted, evMailboxRenamed, evSubscriptionChange:
		return w.WriteList(sess.notifyListData(ev))
	case evMessageNew, evMessageExpunge, evFlagChange:
		// RFC 5465 sections 5.1-5.3: message events in non-selected
		// mailboxes are reported with an unsolicited STATUS response.
		statusOptions := notifyStatusOptions(map[imap.NotifyEvent]bool{ev.kind.notifyEvent(): true}, w.CondStoreEnabled())
		if statusOptions == nil {
			return nil
		}
		mbox, err := sess.user.mailbox(ev.mailbox)
		if err != nil {
			// The mailbox disappeared in the meantime.
			return nil
		}
		return w.WriteStatus(mbox.StatusData(statusOptions), statusOptions)
	}
	return nil
}

// handleSelectedNotifyEvent delivers a message event for the currently
// selected mailbox: pending tracker updates (EXISTS, EXPUNGE, FETCH) are
// flushed, and MessageNew fetch attributes are honored (RFC 5465 section
// 5.2).
func (sess *UserSession) handleSelectedNotifyEvent(ev memNotifyEvent, watch *notifyWatch, mailbox *MailboxView, w *imapserver.UpdateWriter) error {
	// RFC 5465 section 3.1: message events for the selected mailbox are
	// only reported when requested with the SELECTED/SELECTED-DELAYED
	// specifier. Without it, updates stay queued until the next command.
	if !watch.selectedEvents(ev.kind.notifyEvent()) {
		return nil
	}

	// SELECTED-DELAYED delays MessageExpunge until a command sync point
	// (RFC 5465 section 6.1.2): leave expunges queued for the regular
	// per-command Poll. Plain SELECTED delivers immediately, unless a
	// command that forbids expunges is in progress.
	allowExpunge := watch.selected.MailboxSpec == imap.NotifyMailboxSpecSelected && w.ExpungeAllowed()
	if err := mailbox.tracker.Poll(w, allowExpunge); err != nil {
		return err
	}

	if ev.kind == evMessageNew && watch.selected.MessageNewFetch != nil {
		return sess.writeNotifyNewMessages(watch, mailbox, w)
	}
	return nil
}

// writeNotifyNewMessages sends the FETCH responses with the requested
// MessageNew fetch attributes for messages that appeared in the selected
// mailbox since the last notification.
func (sess *UserSession) writeNotifyNewMessages(watch *notifyWatch, mailbox *MailboxView, w *imapserver.UpdateWriter) error {
	type newMessage struct {
		seqNum uint32
		msg    *message
	}

	sess.notifyMutex.Lock()
	since := watch.lastUIDNext
	sess.notifyMutex.Unlock()

	mailbox.mutex.Lock()
	var newMsgs []newMessage
	nextSince := mailbox.uidNext // advance fully if every new message is announced
	for i, msg := range mailbox.l {
		if msg.uid < since {
			continue
		}
		// Only emit fetch-atts for messages the client has already been told
		// about via EXISTS. If a message's EXISTS is still queued (e.g. behind
		// a delayed expunge, or an expunge-unsafe command is in progress),
		// EncodeSeqNum returns 0; emitting a FETCH now would produce a
		// wire-invalid "* 0 FETCH". Stop here and retry from this UID on a
		// later tick, after the EXISTS has been delivered (RFC 5465 §5.2
		// requires EXISTS to precede the FETCH).
		seqNum := mailbox.tracker.EncodeSeqNum(uint32(i) + 1)
		if seqNum == 0 {
			nextSince = msg.uid
			break
		}
		newMsgs = append(newMsgs, newMessage{seqNum: seqNum, msg: msg})
	}
	mailbox.mutex.Unlock()

	// Advance the high-water mark past the announced messages only. A write
	// failure tears the connection down anyway; advancing before the write
	// avoids duplicate FETCHes if delivery partially fails.
	sess.notifyMutex.Lock()
	if nextSince > watch.lastUIDNext {
		watch.lastUIDNext = nextSince
	}
	sess.notifyMutex.Unlock()

	fetchWriter := w.FetchWriter()
	for _, nm := range newMsgs {
		respWriter := fetchWriter.CreateMessage(nm.seqNum)
		if err := nm.msg.fetch(respWriter, watch.selected.MessageNewFetch); err != nil {
			return err
		}
	}
	return nil
}

// notifyEventsForMailbox returns the union of events requested for the given
// mailbox by all non-selected specifiers (RFC 5465 section 6: multiple event
// groups can apply to the same mailbox; SELECTED/SELECTED-DELAYED are the
// only specifiers matching the selected mailbox, which is handled
// separately).
func (sess *UserSession) notifyEventsForMailbox(watch *notifyWatch, name string) map[imap.NotifyEvent]bool {
	events := make(map[imap.NotifyEvent]bool)
	for i := range watch.options.Items {
		item := &watch.options.Items[i]
		match := false
		switch item.MailboxSpec {
		case imap.NotifyMailboxSpecSelected, imap.NotifyMailboxSpecSelectedDelayed:
			continue
		case imap.NotifyMailboxSpecPersonal:
			// The in-memory server only has personal mailboxes.
			match = true
		case imap.NotifyMailboxSpecInboxes:
			// RFC 5465 section 6.3: mailboxes an MDA may deliver to; INBOX
			// for the in-memory server.
			match = strings.EqualFold(name, "INBOX")
		case imap.NotifyMailboxSpecSubscribed:
			if mbox, err := sess.user.mailbox(name); err == nil {
				mbox.mutex.Lock()
				match = mbox.subscribed
				mbox.mutex.Unlock()
			}
		default:
			for _, root := range item.Mailboxes {
				if name == root {
					match = true
					break
				}
				if item.Subtree && strings.HasPrefix(name, root+string(mailboxDelim)) {
					match = true
					break
				}
			}
		}
		if match {
			for _, ev := range item.Events {
				events[ev] = true
			}
		}
	}
	return events
}

// notifyListData builds the unsolicited LIST response for a mailbox event
// (RFC 5465 sections 5.4 and 5.5).
func (sess *UserSession) notifyListData(ev memNotifyEvent) *imap.ListData {
	data := &imap.ListData{
		Mailbox: ev.mailbox,
		Delim:   mailboxDelim,
	}
	switch ev.kind {
	case evMailboxDeleted:
		data.Attrs = append(data.Attrs, imap.MailboxAttrNonExistent)
	case evMailboxRenamed:
		data.OldName = ev.oldName
	}
	// RFC 5465 section 5.5: include \Subscribed if and only if the mailbox
	// is subscribed after the event.
	if ev.kind != evMailboxDeleted {
		if mbox, err := sess.user.mailbox(ev.mailbox); err == nil {
			mbox.mutex.Lock()
			if mbox.subscribed {
				data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
			}
			mbox.mutex.Unlock()
		}
	}
	return data
}

// notifyStatusOptions maps the message events enabled for a non-selected
// mailbox to the STATUS items required by RFC 5465 sections 5.1-5.3, and
// section 3.1 for the initial STATUS responses. It returns nil when no
// message event is enabled (no STATUS response should be sent).
func notifyStatusOptions(events map[imap.NotifyEvent]bool, condStore bool) *imap.StatusOptions {
	options := &imap.StatusOptions{}
	any := false
	if events[imap.NotifyEventMessageNew] {
		// RFC 5465 section 5.2: STATUS (UIDNEXT MESSAGES), UIDVALIDITY for
		// the initial STATUS of section 3.1.
		options.NumMessages = true
		options.UIDNext = true
		options.UIDValidity = true
		any = true
	}
	if events[imap.NotifyEventMessageExpunge] {
		// RFC 5465 section 5.3: STATUS (UIDNEXT MESSAGES).
		options.NumMessages = true
		options.UIDNext = true
		any = true
	}
	if events[imap.NotifyEventFlagChange] {
		// RFC 5465 section 5.1: UIDVALIDITY and HIGHESTMODSEQ when
		// CONDSTORE/QRESYNC is enabled, otherwise UNSEEN (the server MAY
		// include it; without it there would be nothing to report).
		options.UIDValidity = true
		if condStore {
			options.HighestModSeq = true
		} else {
			options.NumUnseen = true
		}
		any = true
	}
	if !any {
		return nil
	}
	if condStore {
		options.HighestModSeq = true
	}
	return options
}

// sortedMailboxNames returns the user's mailbox names in a stable order.
func (u *User) sortedMailboxNames() []string {
	u.mutex.Lock()
	names := make([]string, 0, len(u.mailboxes))
	for name := range u.mailboxes {
		names = append(names, name)
	}
	u.mutex.Unlock()
	sort.Strings(names)
	return names
}
