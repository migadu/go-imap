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
	evMailboxParent   // the direct parent of a created or deleted mailbox
	evMailboxNoAccess // the client kept the 'l' right but lost another one
	evSubscriptionChange
	evMailboxMetadataChange
	evServerMetadataChange
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
	case evMailboxCreated, evMailboxDeleted, evMailboxRenamed, evMailboxParent, evMailboxNoAccess:
		return imap.NotifyEventMailboxName
	case evSubscriptionChange:
		return imap.NotifyEventSubscriptionChange
	case evMailboxMetadataChange:
		return imap.NotifyEventMailboxMetadataChange
	case evServerMetadataChange:
		return imap.NotifyEventServerMetadataChange
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

	// mbox identifies the mailbox a message event happened in. Events are
	// routed by identity rather than by name, so a concurrent RENAME can
	// neither misroute them nor race the name read.
	mbox *Mailbox

	// entries lists the names of the annotations changed by a metadata event,
	// deletions included (RFC 5465 sections 5.6 and 5.7). Unsolicited METADATA
	// responses carry names only (RFC 5464 section 4.4).
	entries []string

	// source is the session that caused the event, when known. RFC 5465
	// section 5: "The server SHOULD omit notifying the client if the event is
	// caused by this client."
	source *UserSession
}

// notifyRegistry fans change events out to the NOTIFY watchers of a user.
// Watcher channels are buffered; events for a watcher that has fallen behind
// are dropped (the in-memory server favors simplicity — a production backend
// would send NOTIFICATIONOVERFLOW instead).
type notifyRegistry struct {
	mutex    sync.Mutex
	watchers map[chan<- memNotifyEvent]*UserSession
}

func (r *notifyRegistry) register(ch chan<- memNotifyEvent, owner *UserSession) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.watchers == nil {
		r.watchers = make(map[chan<- memNotifyEvent]*UserSession)
	}
	r.watchers[ch] = owner
}

func (r *notifyRegistry) unregister(ch chan<- memNotifyEvent) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	delete(r.watchers, ch)
}

func (r *notifyRegistry) broadcast(ev memNotifyEvent) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for ch, owner := range r.watchers {
		// RFC 5465 section 5: don't notify the session that caused the event.
		if ev.source != nil && owner == ev.source {
			continue
		}
		select {
		case ch <- ev:
		default:
			// Watcher queue full: drop the event.
		}
	}
}

// maxNotifyQueuedUpdates bounds how many undelivered updates the selected
// mailbox may accumulate while the client's watch suppresses message events for
// it. Past that, the in-memory server declares a notification overflow (RFC
// 5465 section 5.8) rather than let a frozen view grow without bound.
const maxNotifyQueuedUpdates = 1024

// memNotifySupportedEvents is the set of RFC 5465 events the in-memory
// server supports, advertised in the BADEVENT response code.
var memNotifySupportedEvents = []imap.NotifyEvent{
	imap.NotifyEventMessageNew,
	imap.NotifyEventMessageExpunge,
	imap.NotifyEventFlagChange,
	imap.NotifyEventMailboxName,
	imap.NotifyEventSubscriptionChange,
	imap.NotifyEventMailboxMetadataChange,
	imap.NotifyEventServerMetadataChange,
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
		// RFC 5465 section 3.1: no events at all. The watch goes, but the
		// event channel stays registered: the selected mailbox's updates keep
		// queuing behind the suppression, and NotifyPoll — which the library
		// runs for NOTIFY NONE too — needs the wake-ups to notice when that
		// queue must be cut short (checkNotifyOverflow).
		sess.notifyMutex.Lock()
		sess.notifyWatch = nil
		sess.startNotifyLocked()
		sess.notifyMutex.Unlock()
		return nil
	}

	// A rejected watch leaves the previous state in effect, the pre-NOTIFY one
	// included, so nothing is touched before the events have been checked.
	for _, item := range options.Items {
		for _, ev := range item.Events {
			switch ev {
			case imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge,
				imap.NotifyEventFlagChange, imap.NotifyEventMailboxName,
				imap.NotifyEventSubscriptionChange,
				imap.NotifyEventMailboxMetadataChange, imap.NotifyEventServerMetadataChange:
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
	mailbox := sess.mailbox
	sess.notifyMutex.Unlock()

	// name and uidNext are guarded by the mailbox lock, not by notifyMutex:
	// another session may be appending or renaming concurrently.
	selectedName := ""
	if mailbox != nil {
		mailbox.mutex.Lock()
		selectedName = mailbox.name
		watch.lastUIDNext = mailbox.uidNext
		mailbox.mutex.Unlock()
	}

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

	// Start capturing events before the watch goes live, so nothing raised
	// between here and the first NotifyPoll is missed.
	sess.notifyMutex.Lock()
	sess.startNotifyLocked()
	sess.notifyWatch = watch
	sess.notifyMutex.Unlock()
	return nil
}

// startNotifyLocked puts the session under NOTIFY's delivery rules (see
// notifyOff) and registers its event channel with the user, unless both are
// already the case. notifyMutex must be held.
func (sess *UserSession) startNotifyLocked() {
	if sess.notifyOff == nil {
		sess.notifyOff = make(chan struct{})
	}
	if sess.notifyEvents == nil {
		sess.notifyEvents = make(chan memNotifyEvent, 256)
		sess.user.notify.register(sess.notifyEvents, sess)
	}
}

// stopNotifyEvents drops the watch and stops capturing events for it.
func (sess *UserSession) stopNotifyEvents() {
	sess.notifyMutex.Lock()
	ch := sess.notifyEvents
	sess.notifyEvents = nil
	sess.notifyWatch = nil
	sess.notifyMutex.Unlock()

	if ch != nil {
		sess.user.notify.unregister(ch)
	}
}

// NotifyPoll implements imapserver.SessionNotify.
//
// It runs after NOTIFY NONE as well, with no watch: then it delivers nothing
// and only keeps the selected mailbox's queue of suppressed updates within
// bounds (checkNotifyOverflow).
func (sess *UserSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	// The channel is registered by SetNotify and outlives this call: the
	// library stops and restarts the pump around every change of the selected
	// mailbox, and events raised in that window must be queued, not lost. It
	// is nil once NOTIFY is over — after a notification overflow, or when
	// NOTIFY was never used on this session.
	sess.notifyMutex.Lock()
	ch := sess.notifyEvents
	sess.notifyMutex.Unlock()
	if ch == nil {
		return nil
	}

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
			overflowed, err := sess.checkNotifyOverflow(w)
			if err != nil {
				return err
			}
			if overflowed {
				// RFC 5465 section 5.8: the server behaves as if NOTIFY NONE
				// had been received, so the watch is gone and there is nothing
				// left to pump.
				return nil
			}
		}
	}
}

// checkNotifyOverflow declares a notification overflow when the selected
// mailbox has accumulated more undeliverable updates than the server is willing
// to hold (RFC 5465 section 5.8). It reports whether the watch was dropped.
//
// The library then returns the connection to pre-NOTIFY delivery, so this
// session does the same: the watch and the event channel go, and notifyOff is
// closed so that an IDLE in progress resumes pushing updates.
func (sess *UserSession) checkNotifyOverflow(w *imapserver.UpdateWriter) (bool, error) {
	sess.notifyMutex.Lock()
	mailbox := sess.mailbox
	sess.notifyMutex.Unlock()
	if mailbox == nil || mailbox.tracker.QueuedUpdates() <= maxNotifyQueuedUpdates {
		return false, nil
	}

	if err := w.WriteNotificationOverflow(); err != nil {
		return false, err
	}
	sess.stopNotifyEvents()

	sess.notifyMutex.Lock()
	if sess.notifyOff != nil {
		close(sess.notifyOff)
		sess.notifyOff = nil
	}
	sess.notifyMutex.Unlock()
	return true, nil
}

func (sess *UserSession) handleNotifyEvent(ev memNotifyEvent, w *imapserver.UpdateWriter) error {
	sess.notifyMutex.Lock()
	watch := sess.notifyWatch
	mailbox := sess.mailbox
	sess.notifyMutex.Unlock()
	if watch == nil {
		return nil
	}

	// Compare mailbox identity, not names: a concurrent RENAME would both race
	// the name read and misroute the event.
	if ev.kind.isMessageEvent() && mailbox != nil && ev.mbox != nil && ev.mbox == mailbox.Mailbox {
		return sess.handleSelectedNotifyEvent(ev, watch, mailbox, w)
	}

	// RFC 5465 sections 5.7 and 8: ServerMetadataChange is a <user-event>, not
	// tied to a mailbox, so it is enabled by any non-selected event group.
	if ev.kind == evServerMetadataChange {
		if !notifyEventRequested(watch, imap.NotifyEventServerMetadataChange) {
			return nil
		}
		return w.WriteMetadata("", ev.entries)
	}

	events := sess.notifyEventsForMailbox(watch, ev.mailbox)
	if !events[ev.kind.notifyEvent()] {
		return nil
	}

	switch ev.kind {
	case evMailboxCreated, evMailboxDeleted, evMailboxRenamed, evMailboxParent, evMailboxNoAccess, evSubscriptionChange:
		return w.WriteList(sess.notifyListData(ev))
	case evMailboxMetadataChange:
		// RFC 5465 section 5.6: report the changed annotations with an
		// unsolicited METADATA response.
		return w.WriteMetadata(ev.mailbox, ev.entries)
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

	// SELECTED-DELAYED delays MessageExpunge until the client issues a command
	// that allows reporting expunged messages (RFC 5465 section 6.1.2) — NOOP
	// or IDLE, which DelayedExpungeAllowed reports. Withholding them past that
	// would also head-of-line block every later EXISTS, since updates are
	// delivered in order. Plain SELECTED delivers immediately, unless a command
	// that forbids expunges is in progress.
	var allowExpunge bool
	if watch.selected.MailboxSpec == imap.NotifyMailboxSpecSelected {
		allowExpunge = w.ExpungeAllowed()
	} else {
		allowExpunge = w.DelayedExpungeAllowed()
	}

	// The fetch-atts of the new messages are written while the delivery lock is
	// still held: their sequence numbers are only valid for the state this Poll
	// just delivered.
	var after func() error
	if watch.selected.MessageNewFetch != nil {
		after = func() error { return sess.writeNotifyNewMessages(watch, mailbox, w) }
	}
	return mailbox.tracker.PollWith(w, allowExpunge, after)
}

// notifyPendingFetch writes any MessageNew fetch-atts that could not be sent
// yet — a message whose EXISTS was still queued has no client-side sequence
// number. It is called from the per-command sync point as well as from the
// pump, because the sync point is what releases those queued updates and the
// pump only wakes on new events.
func (sess *UserSession) notifyPendingFetch(w *imapserver.UpdateWriter) error {
	sess.notifyMutex.Lock()
	watch, mailbox := sess.notifyWatch, sess.mailbox
	sess.notifyMutex.Unlock()
	if watch == nil || mailbox == nil || watch.selected == nil || watch.selected.MessageNewFetch == nil {
		return nil
	}
	if !watch.selectedEvents(imap.NotifyEventMessageNew) {
		return nil
	}
	return mailbox.tracker.PollWith(w, false, func() error {
		return sess.writeNotifyNewMessages(watch, mailbox, w)
	})
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

	// The message fields the fetch renders (flags, modSeq, body) are guarded by
	// the mailbox lock, so it is held across the writes. MailboxView.Fetch does
	// the same, and nothing the fetch calls takes the mailbox lock again.
	defer mailbox.mutex.Unlock()

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
		// RFC 5465 section 5.2: no FETCH response for a message this
		// connection created itself.
		if sess.takeOwnMessage(nm.msg.uid) {
			continue
		}
		respWriter := fetchWriter.CreateMessage(nm.seqNum)
		if err := nm.msg.fetch(respWriter, watch.selected.MessageNewFetch); err != nil {
			return err
		}
	}
	return nil
}

// notifyEventRequested reports whether any non-selected event group requests
// the given event, regardless of the mailboxes it applies to. It is used for
// server-wide events, which are not tied to a mailbox.
func notifyEventRequested(watch *notifyWatch, event imap.NotifyEvent) bool {
	for i := range watch.options.Items {
		item := &watch.options.Items[i]
		if item.MailboxSpec == imap.NotifyMailboxSpecSelected ||
			item.MailboxSpec == imap.NotifyMailboxSpecSelectedDelayed {
			continue
		}
		for _, ev := range item.Events {
			if ev == event {
				return true
			}
		}
	}
	return false
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
	if ev.kind == evMailboxRenamed {
		data.OldName = ev.oldName
	}
	if ev.kind == evMailboxNoAccess {
		// RFC 5465 section 5.9: report the loss of a right needed to monitor
		// the mailbox, which the client can still see.
		data.Attrs = append(data.Attrs, imap.MailboxAttrNoAccess)
	}

	mbox, err := sess.user.mailbox(ev.mailbox)
	switch {
	case ev.kind == evMailboxDeleted || err != nil:
		// RFC 5465 section 5.4: "If, after the event, the mailbox name does not
		// refer to a mailbox accessible to the client, the \Nonexistent flag
		// MUST be included." A parent reported as affected by the creation or
		// deletion of a child need not exist itself.
		data.Attrs = append(data.Attrs, imap.MailboxAttrNonExistent)
	default:
		// RFC 5465 section 5.5: include \Subscribed if and only if the mailbox
		// is subscribed after the event.
		mbox.mutex.Lock()
		if mbox.subscribed {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		mbox.mutex.Unlock()
	}

	// RFC 5465 section 5.4: the server SHOULD also report whether the mailbox
	// has children.
	if sess.user.hasChildren(ev.mailbox) {
		data.Attrs = append(data.Attrs, imap.MailboxAttrHasChildren)
	} else {
		data.Attrs = append(data.Attrs, imap.MailboxAttrHasNoChildren)
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
