package imapserver

import (
	"fmt"
	"sync"

	"github.com/emersion/go-imap/v2"
)

// MailboxTracker tracks the state of a mailbox.
//
// A mailbox can have multiple sessions listening for updates. Each session has
// its own view of the mailbox, because IMAP clients asynchronously receive
// mailbox updates.
type MailboxTracker struct {
	mutex       sync.Mutex
	numMessages uint32
	sessions    map[*SessionTracker]struct{}
}

// NewMailboxTracker creates a new mailbox tracker.
func NewMailboxTracker(numMessages uint32) *MailboxTracker {
	return &MailboxTracker{
		numMessages: numMessages,
		sessions:    make(map[*SessionTracker]struct{}),
	}
}

// NewSession creates a new session tracker for the mailbox.
//
// The caller must call SessionTracker.Close once they are done with the
// session.
func (t *MailboxTracker) NewSession() *SessionTracker {
	st := &SessionTracker{mailbox: t}
	t.mutex.Lock()
	t.sessions[st] = struct{}{}
	t.mutex.Unlock()
	return st
}

func (t *MailboxTracker) queueUpdate(update *trackerUpdate, source *SessionTracker) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	// These are backend invariant violations. Return an error instead of
	// panicking: MailboxTracker is a shared entry point that backends call from
	// arbitrary goroutines (not just IDLE, which recovers), so a panic here would
	// crash whatever goroutine reported the inconsistency — often the whole
	// process.
	if update.expunge != 0 && update.expunge > t.numMessages {
		return fmt.Errorf("imapserver: expunge sequence number (%v) out of range (%v messages in mailbox)", update.expunge, t.numMessages)
	}
	if update.numMessages != 0 && update.numMessages < t.numMessages {
		return fmt.Errorf("imapserver: cannot decrease mailbox number of messages from %v to %v", t.numMessages, update.numMessages)
	}

	// Capture the current count before applying the update so that
	// EncodeSeqNum can determine which seqNums are new (not yet seen
	// by the client).
	if update.numMessages != 0 {
		update.prevNumMessages = t.numMessages
	}

	for st := range t.sessions {
		if source != nil && st == source {
			continue
		}
		st.queueUpdate(update)
	}

	switch {
	case update.expunge != 0:
		t.numMessages--
	case update.numMessages != 0:
		t.numMessages = update.numMessages
	}
	return nil
}

// QueueExpunge queues a new EXPUNGE update.
//
// uid is the UID of the expunged message. It is used to emit a VANISHED response
// (RFC 7162 §3.2.10) instead of EXPUNGE for QRESYNC-enabled sessions; pass 0 when
// no UID is available (such sessions then fall back to EXPUNGE).
//
// It returns an error (rather than panicking) if seqNum is 0 or out of range, so
// a backend bug cannot crash the calling goroutine.
func (t *MailboxTracker) QueueExpunge(seqNum uint32, uid imap.UID) error {
	if seqNum == 0 {
		return fmt.Errorf("imapserver: invalid expunge message sequence number")
	}
	return t.queueUpdate(&trackerUpdate{expunge: seqNum, expungeUID: uid}, nil)
}

// QueueNumMessages queues a new EXISTS update. It returns an error if n would
// decrease the tracked message count.
func (t *MailboxTracker) QueueNumMessages(n uint32) error {
	// TODO: merge consecutive NumMessages updates
	return t.queueUpdate(&trackerUpdate{numMessages: n}, nil)
}

// QueueMailboxFlags queues a new FLAGS update.
func (t *MailboxTracker) QueueMailboxFlags(flags []imap.Flag) error {
	if flags == nil {
		flags = []imap.Flag{}
	}
	return t.queueUpdate(&trackerUpdate{mailboxFlags: flags}, nil)
}

// QueueMessageFlags queues a new FETCH FLAGS update.
//
// modSeq is the modification sequence (RFC 7162) of the flag change. When non-zero
// and the receiving session is CONDSTORE-aware, it is emitted as a MODSEQ data item
// in the unsolicited FETCH response so the client can advance its per-message modseq
// instead of falling back to a full re-sync. Pass 0 when no modseq is available.
//
// If source is not nil, the update won't be dispatched to it.
func (t *MailboxTracker) QueueMessageFlags(seqNum uint32, uid imap.UID, flags []imap.Flag, modSeq uint64, source *SessionTracker) error {
	return t.queueUpdate(&trackerUpdate{fetch: &trackerUpdateFetch{
		seqNum: seqNum,
		uid:    uid,
		flags:  flags,
		modSeq: modSeq,
	}}, source)
}

type trackerUpdate struct {
	expunge         uint32
	expungeUID      imap.UID // UID of the expunged message; enables VANISHED (RFC 7162) for QRESYNC sessions
	numMessages     uint32
	prevNumMessages uint32 // numMessages before this update; used by EncodeSeqNum
	mailboxFlags    []imap.Flag
	fetch           *trackerUpdateFetch
}

type trackerUpdateFetch struct {
	seqNum uint32
	uid    imap.UID
	flags  []imap.Flag
	modSeq uint64
}

// SessionTracker tracks the state of a mailbox for an IMAP client.
type SessionTracker struct {
	mailbox *MailboxTracker

	mutex   sync.Mutex
	queue   []trackerUpdate
	updates chan<- struct{}
}

// Close unregisters the session.
func (t *SessionTracker) Close() {
	t.mailbox.mutex.Lock()
	delete(t.mailbox.sessions, t)
	t.mailbox.mutex.Unlock()
	t.mailbox = nil
}

func (t *SessionTracker) queueUpdate(update *trackerUpdate) {
	var updates chan<- struct{}
	t.mutex.Lock()
	t.queue = append(t.queue, *update)
	updates = t.updates
	t.mutex.Unlock()

	if updates != nil {
		select {
		case updates <- struct{}{}:
			// we notified SessionTracker.Idle about the update
		default:
			// skip the update
		}
	}
}

// Poll dequeues pending mailbox updates for this session.
func (t *SessionTracker) Poll(w *UpdateWriter, allowExpunge bool) error {
	var updates []trackerUpdate
	t.mutex.Lock()
	if allowExpunge {
		updates = t.queue
		t.queue = nil
	} else {
		stopIndex := -1
		for i, update := range t.queue {
			if update.expunge != 0 {
				stopIndex = i
				break
			}
			updates = append(updates, update)
		}
		if stopIndex >= 0 {
			t.queue = t.queue[stopIndex:]
		} else {
			t.queue = nil
		}
	}
	t.mutex.Unlock()

	// RFC 7162 §3.2.10: once QRESYNC is enabled, expunges are reported as
	// VANISHED (by UID) rather than EXPUNGE (by sequence number). Coalesce
	// consecutive expunges into a single VANISHED set, flushing it before any
	// other kind of update so response ordering is preserved.
	qresync := w.conn != nil && w.conn.enabledHas(imap.CapQResync)
	var vanished imap.UIDSet
	flushVanished := func() error {
		if len(vanished) == 0 {
			return nil
		}
		uids := vanished
		vanished = nil
		return w.WriteVanished(uids)
	}

	for _, update := range updates {
		var err error
		switch {
		case update.expunge != 0:
			// A QRESYNC session with a known UID gets VANISHED; otherwise fall
			// back to the classic EXPUNGE (by sequence number).
			if qresync && update.expungeUID != 0 {
				vanished.AddNum(update.expungeUID)
				continue
			}
			if err = flushVanished(); err != nil {
				return err
			}
			err = w.WriteExpunge(update.expunge)
		case update.numMessages != 0:
			if err = flushVanished(); err != nil {
				return err
			}
			err = w.WriteNumMessages(update.numMessages)
		case update.mailboxFlags != nil:
			if err = flushVanished(); err != nil {
				return err
			}
			err = w.WriteMailboxFlags(update.mailboxFlags)
		case update.fetch != nil:
			if err = flushVanished(); err != nil {
				return err
			}
			err = w.WriteMessageFlags(update.fetch.seqNum, update.fetch.uid, update.fetch.flags, update.fetch.modSeq)
		default:
			panic(fmt.Errorf("imapserver: unknown tracker update %#v", update))
		}
		if err != nil {
			return err
		}
	}
	return flushVanished()
}

// Idle continuously writes mailbox updates.
//
// When the stop channel is closed, it returns.
//
// Idle cannot be invoked concurrently from two separate goroutines.
func (t *SessionTracker) Idle(w *UpdateWriter, stop <-chan struct{}) error {
	updates := make(chan struct{}, 64)
	t.mutex.Lock()
	ok := t.updates == nil
	if ok {
		t.updates = updates
	}
	t.mutex.Unlock()
	if !ok {
		return fmt.Errorf("imapserver: only a single SessionTracker.Idle call is allowed at a time")
	}

	defer func() {
		t.mutex.Lock()
		t.updates = nil
		t.mutex.Unlock()
	}()

	for {
		select {
		case <-updates:
			if err := t.Poll(w, true); err != nil {
				return err
			}
		case <-stop:
			return nil
		}
	}
}

// DecodeSeqNum converts a message sequence number from the client view to the
// server view.
//
// Zero is returned if the message doesn't exist from the server point-of-view.
func (t *SessionTracker) DecodeSeqNum(seqNum uint32) uint32 {
	if seqNum == 0 {
		return 0
	}

	t.mutex.Lock()
	defer t.mutex.Unlock()

	for _, update := range t.queue {
		if update.expunge == 0 {
			continue
		}
		if seqNum == update.expunge {
			return 0
		} else if seqNum > update.expunge {
			seqNum--
		}
	}

	if seqNum > t.mailbox.numMessages {
		return 0
	}

	return seqNum
}

// EncodeSeqNum converts a message sequence number from the server view to the
// client view.
//
// Zero is returned if the message doesn't exist from the client point-of-view.
func (t *SessionTracker) EncodeSeqNum(seqNum uint32) uint32 {
	if seqNum == 0 {
		return 0
	}

	t.mutex.Lock()
	defer t.mutex.Unlock()

	if seqNum > t.mailbox.numMessages {
		return 0
	}

	for i := len(t.queue) - 1; i >= 0; i-- {
		update := t.queue[i]
		if update.numMessages != 0 {
			// Messages added by this update occupy positions
			//   (prevNumMessages+1) .. numMessages
			// from the server's perspective at the time of the append.
			// The client has not seen these messages yet, so they have no
			// client-side sequence number.
			if seqNum > update.prevNumMessages && seqNum <= update.numMessages {
				return 0
			}
			// seqNum <= prevNumMessages: the message existed before this
			// append, so no adjustment is needed for this update.
		}
		if update.expunge != 0 && seqNum >= update.expunge {
			seqNum++
		}
	}
	return seqNum
}
