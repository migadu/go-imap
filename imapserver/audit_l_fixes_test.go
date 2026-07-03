package imapserver

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// L1: deeply nested NOT/OR SEARCH keys must be rejected with a clean error
// rather than recursing unbounded (they bypass the decoder's maxListDepth).
func TestSearchKeyNestingBounded(t *testing.T) {
	input := strings.Repeat("NOT ", maxSearchKeyDepth+50) + "ALL\r\n"
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(input)), imapwire.ConnSideServer)

	var criteria imap.SearchCriteria
	err := readSearchKey(&Conn{}, &criteria, dec)
	if err == nil {
		t.Fatal("expected an error for over-deep SEARCH key nesting, got nil")
	}
	if !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("error = %v, want a nesting-too-deep client bug error", err)
	}
}

// A legitimately shallow nesting must still parse fine.
func TestSearchKeyShallowNestingOK(t *testing.T) {
	input := "OR NOT ALL (SEEN)\r\n"
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(input)), imapwire.ConnSideServer)

	var criteria imap.SearchCriteria
	if err := readSearchKey(&Conn{}, &criteria, dec); err != nil {
		t.Fatalf("readSearchKey() on a shallow query = %v", err)
	}
}

type unauthProbeSession struct {
	Session
}

func (unauthProbeSession) Unauthenticate(ctx context.Context) error { return nil }

// L4: UNAUTHENTICATE must reset the CONDSTORE-enabled flag (RFC 8437), not just
// the enabled cap set, so a re-authenticated client isn't sent MODSEQ items.
func TestUnauthenticateResetsCondStore(t *testing.T) {
	c := &Conn{
		state:   imap.ConnStateAuthenticated,
		enabled: make(imap.CapSet),
		session: unauthProbeSession{},
	}
	c.markCondStoreEnabled()
	if !c.CondStoreEnabled() {
		t.Fatal("precondition: CondStoreEnabled() should be true after markCondStoreEnabled")
	}

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("\r\n")), imapwire.ConnSideServer)
	if err := c.handleUnauthenticate(dec); err != nil {
		t.Fatalf("handleUnauthenticate() = %v", err)
	}

	if c.CondStoreEnabled() {
		t.Error("CondStoreEnabled() still true after UNAUTHENTICATE")
	}
	if c.state != imap.ConnStateNotAuthenticated {
		t.Errorf("state = %v, want NotAuthenticated", c.state)
	}
}

// L5: MailboxTracker invariant violations must return an error, not panic
// (backends call these from arbitrary goroutines with no recover).
func TestMailboxTrackerQueueReturnsErrorNotPanic(t *testing.T) {
	tr := NewMailboxTracker(5)

	if err := tr.QueueExpunge(0, 0); err == nil {
		t.Error("QueueExpunge(0) should return an error")
	}
	if err := tr.QueueExpunge(10, 0); err == nil {
		t.Error("QueueExpunge out of range should return an error")
	}
	if err := tr.QueueNumMessages(3); err == nil {
		t.Error("QueueNumMessages decreasing the count should return an error")
	}
	// A valid update still succeeds.
	if err := tr.QueueNumMessages(7); err != nil {
		t.Errorf("QueueNumMessages(7) on a 5-message mailbox = %v, want nil", err)
	}
}

// L2: enabledHas must be safe to call concurrently with ENABLE writing the
// c.enabled map. Meaningful under -race.
func TestEnabledHasConcurrentSafe(t *testing.T) {
	c := &Conn{enabled: make(imap.CapSet)}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			c.mutex.Lock()
			c.enabled[imap.CapIMAP4rev2] = struct{}{}
			c.mutex.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = c.enabledHas(imap.CapIMAP4rev2)
		}
	}()
	wg.Wait()
}
