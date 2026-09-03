package imapserver

import (
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// The mailbox's message count used to be read by SessionTracker methods holding
// the SESSION's mutex and written by queueUpdate holding the MAILBOX's — one
// field, two mutexes, a data race on any mailbox with two clients in it. The
// readers could not take the mailbox mutex (queueUpdate holds it and then takes
// each session's, so the reverse order deadlocks), and making the field atomic
// only made the read well-defined: the store landed after the fan-out, so a
// reader could see an expunge in its queue that the count had not absorbed.
//
// Each session now carries its own count, written with the queue append in one
// critical section. This test runs the shape that matters under -race — one
// session expunging while others encode — and the tests below assert the view
// invariants the atomic could not deliver.
func TestMailboxTrackerConcurrentExpungeAndEncode(t *testing.T) {
	const (
		messages = 200
		readers  = 4
		reads    = 500
	)
	mbox := NewMailboxTracker(messages)

	writer := mbox.NewSession()
	defer writer.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Expunge from the end so every sequence number stays in range.
		for n := uint32(messages); n > messages/2; n-- {
			_ = mbox.QueueExpunge(n, imap.UID(n))
		}
	}()

	for i := 0; i < readers; i++ {
		sess := mbox.NewSession()
		wg.Add(1)
		go func(st *SessionTracker) {
			defer wg.Done()
			defer st.Close()
			for n := 0; n < reads; n++ {
				seq := uint32(n%messages) + 1
				_ = st.EncodeSeqNum(seq)
				_ = st.DecodeSeqNum(seq)
				_ = st.EncodeNumMessages()
			}
		}(sess)
	}
	wg.Wait()
}

// A concurrent EXISTS/expunge mix, which additionally exercises the
// read-check-write inside queueUpdate: two writers must not both pass the range
// check and then drive the count below zero.
func TestMailboxTrackerConcurrentWriters(t *testing.T) {
	mbox := NewMailboxTracker(1)
	sess := mbox.NewSession()
	defer sess.Close()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(base uint32) {
			defer wg.Done()
			for n := uint32(0); n < 200; n++ {
				// QueueNumMessages refuses a decrease, so climb monotonically.
				_ = mbox.QueueNumMessages(base*1000 + n + 1)
				_ = sess.EncodeNumMessages()
			}
		}(uint32(i))
	}
	wg.Wait()
}

// With only expunges in flight, a session's view can never grow: the largest
// sequence number the client can name stays at the initial count until it is
// told otherwise, and a client number past it never decodes to a message.
//
// Under the atomic this failed in tens of iterations per run: a reader found the
// expunge in its queue and the pre-decrement count, undid the expunge, and
// answered initial+1 — a "*" pointing one past the client's own view. It is a
// logic window, not a data race, so the detector never saw it.
func TestSessionTrackerViewNeverAheadOfQueue(t *testing.T) {
	const messages = 1000
	for round := 0; round < 100; round++ {
		mbox := NewMailboxTracker(messages)
		reader := mbox.NewSession()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for n := uint32(messages); n > messages/2; n-- {
				_ = mbox.QueueExpunge(n, imap.UID(n))
			}
		}()
		var overshoot, decodedPastEnd int
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if v := reader.EncodeNumMessages(); v > messages {
					overshoot++
				}
				if reader.DecodeSeqNum(messages+1) != 0 {
					decodedPastEnd++
				}
			}
		}()
		wg.Wait()
		reader.Close()

		if overshoot != 0 || decodedPastEnd != 0 {
			t.Fatalf("round %d: EncodeNumMessages exceeded the client's view %d times, DecodeSeqNum(max+1) found a message %d times", round, overshoot, decodedPastEnd)
		}
	}
}

// The source session of a flag update is spared the update but must still
// track the count: its view is the server's, with nothing pending.
func TestSessionTrackerSourceKeepsCount(t *testing.T) {
	mbox := NewMailboxTracker(3)
	source := mbox.NewSession()
	defer source.Close()
	other := mbox.NewSession()
	defer other.Close()

	if err := mbox.QueueMessageFlags(2, 20, []imap.Flag{imap.FlagSeen}, 0, source); err != nil {
		t.Fatal(err)
	}
	if got := source.QueuedUpdates(); got != 0 {
		t.Fatalf("source queued %d updates, want 0", got)
	}
	if got := other.QueuedUpdates(); got != 1 {
		t.Fatalf("other queued %d updates, want 1", got)
	}
	if got := source.EncodeNumMessages(); got != 3 {
		t.Fatalf("source EncodeNumMessages = %d, want 3", got)
	}
	// Ensure the count reaches a spared source even when an update does move it,
	// should a future update kind carry a source: drive it through the
	// internal entry point directly.
	if err := mbox.queueUpdate(&trackerUpdate{numMessages: 5}, source); err != nil {
		t.Fatal(err)
	}
	if got := source.EncodeNumMessages(); got != 5 {
		t.Fatalf("source EncodeNumMessages after spared EXISTS = %d, want 5", got)
	}
	if got := source.EncodeSeqNum(5); got != 5 {
		t.Fatalf("source EncodeSeqNum(5) = %d, want 5", got)
	}
}

// After Close every reader answers 0 rather than reporting a view of a mailbox
// the session has left; a NOTIFY pump may still hold the tracker. Before the
// guard, EncodeNumMessages dereferenced the released mailbox here.
func TestSessionTrackerReadersAfterClose(t *testing.T) {
	mbox := NewMailboxTracker(3)
	sess := mbox.NewSession()
	if err := mbox.QueueExpunge(1, 10); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	if got := sess.EncodeNumMessages(); got != 0 {
		t.Fatalf("EncodeNumMessages after Close = %d, want 0", got)
	}
	if got := sess.EncodeSeqNum(1); got != 0 {
		t.Fatalf("EncodeSeqNum after Close = %d, want 0", got)
	}
	if got := sess.DecodeSeqNum(1); got != 0 {
		t.Fatalf("DecodeSeqNum after Close = %d, want 0", got)
	}
}
