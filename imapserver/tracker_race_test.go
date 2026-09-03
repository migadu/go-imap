package imapserver

import (
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// MailboxTracker.numMessages is read by SessionTracker methods that hold the
// SESSION's mutex, and written by queueUpdate holding the MAILBOX's. Guarding
// one field with two mutexes is a data race, and it is reachable on any server
// with more than one client in a mailbox: one session expunging while another
// encodes a sequence number is the ordinary case, not a corner.
//
// It cannot be fixed by taking the mailbox mutex in the readers: queueUpdate
// holds that mutex and then takes each session's mutex to enqueue, so the
// readers acquiring them in the opposite order would deadlock. Hence the atomic.
//
// Run under -race; without it this test passes whatever the field's type,
// because the values stay plausible either way. That is the point — the defect
// this pins is invisible to assertions.
func TestMailboxTrackerConcurrentExpungeAndEncode(t *testing.T) {
	const (
		messages = 200
		readers  = 4
		reads    = 500
	)
	mbox := NewMailboxTracker(messages)

	// One session does the expunging; the others only read, which is the shape
	// that matters — a reader must not have to synchronise with a writer it
	// knows nothing about.
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
