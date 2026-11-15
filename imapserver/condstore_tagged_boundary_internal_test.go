package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

type condStoreCompletionGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newCondStoreCompletionGate(t *testing.T) *condStoreCompletionGate {
	g := &condStoreCompletionGate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *condStoreCompletionGate) wait() {
	close(g.entered)
	<-g.release
}

func (g *condStoreCompletionGate) open() {
	g.once.Do(func() { close(g.release) })
}

// The session has reported every change through 3 and still holds expunge 4.
// Flag changes above that gap can be delivered during FETCH, or while
// SELECTED-DELAYED withholds the expunge during ID and between commands.
type condStoreCompletionSession struct {
	Session
	conn        *Conn
	fetchModSeq uint64
	highestGate *condStoreCompletionGate
	idGate      *condStoreCompletionGate
}

func (s *condStoreCompletionSession) Poll(context.Context, *UpdateWriter, bool) error {
	return nil
}

func (s *condStoreCompletionSession) Fetch(_ context.Context, w *FetchWriter, _ imap.NumSet, _ *imap.FetchOptions) error {
	f := w.CreateMessage(1)
	f.WriteUID(1)
	f.WriteModSeq(s.fetchModSeq)
	return f.Close()
}

func (s *condStoreCompletionSession) HighestModSeq() uint64 {
	if gate := s.highestGate; gate != nil {
		s.highestGate = nil
		gate.wait()
	}
	return 3
}

func (s *condStoreCompletionSession) ID(*imap.IDData) *imap.IDData {
	if gate := s.idGate; gate != nil {
		s.idGate = nil
		gate.wait()
	}
	return nil
}

type condStoreCompletionNotification struct {
	attempted chan struct{}
	done      chan struct{}
	err       error // read only after done closes
}

func (s *condStoreCompletionSession) notify(modSeq uint64) *condStoreCompletionNotification {
	n := &condStoreCompletionNotification{attempted: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(n.done)
		w := &UpdateWriter{conn: s.conn, notify: true, allowExpunge: true}
		s.conn.cmdMutex.Lock()
		command := s.conn.activeCmd
		s.conn.cmdMutex.Unlock()
		if command == "FETCH" && w.ExpungeAllowed() {
			n.err = fmt.Errorf("FETCH unexpectedly permits the held expunge")
		} else if command != "FETCH" && w.DelayedExpungeAllowed() {
			n.err = fmt.Errorf("%q unexpectedly releases the SELECTED-DELAYED expunge", command)
		}
		close(n.attempted)
		if n.err == nil {
			n.err = w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, modSeq)
		}
	}()
	return n
}

func newCondStoreCompletionConn(t *testing.T) (*Conn, *condStoreCompletionSession, *bytes.Buffer) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	var out bytes.Buffer
	s := &condStoreCompletionSession{fetchModSeq: 2}
	c := &Conn{
		conn:    a,
		bw:      bufio.NewWriter(&out),
		ctx:     context.Background(),
		state:   imap.ConnStateSelected,
		session: s,
		enabled: imap.CapSet{imap.CapQResync: {}},
		server: New(&Options{Caps: imap.CapSet{
			imap.CapIMAP4rev1: {}, imap.CapCondStore: {}, imap.CapQResync: {}, imap.CapID: {},
		}}),
		notifyUsed: true,
		notifySelectedEvents: map[imap.NotifyEvent]bool{
			imap.NotifyEventFlagChange: true, imap.NotifyEventMessageExpunge: true,
		},
	}
	s.conn = c
	return c, s, &out
}

func runCondStoreCompletionCommand(c *Conn, command string) <-chan error {
	done := make(chan error, 1)
	go func() {
		dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(command+"\r\n")), imapwire.ConnSideServer)
		done <- c.readCommand(dec)
	}()
	return done
}

func awaitCondStoreCompletion(t *testing.T, done <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func awaitCondStoreCompletionCommand(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for command completion")
	}
}

func requireCondStoreCompletionWatermark(t *testing.T, span, tag string, modSeq uint64) {
	t.Helper()
	tagIndex := strings.Index(span, tag+" OK ")
	modSeqIndex := strings.Index(span, fmt.Sprintf("MODSEQ (%d)", modSeq))
	if tagIndex < 0 || modSeqIndex < 0 || modSeqIndex >= tagIndex {
		t.Fatalf("want MODSEQ %d before %s completion, got:\n%s", modSeq, tag, span)
	}
	span = span[:tagIndex]
	watermarkIndex := strings.Index(span, "[HIGHESTMODSEQ 3]")
	if watermarkIndex < modSeqIndex || strings.Count(span, "[HIGHESTMODSEQ ") != 1 {
		t.Fatalf("MODSEQ %d above held expunge 4 needs HIGHESTMODSEQ 3 before %s completion, got:\n%s", modSeq, tag, span)
	}
}

// A notification arriving while the backend reports its safe watermark must
// belong wholly to this tagged span or to the following one (RFC 7162 section
// 6). In particular, a MODSEQ snapshot taken before the query cannot miss it.
func TestCondStoreConcurrentModSeqTaggedBoundary(t *testing.T) {
	c, s, out := newCondStoreCompletionConn(t)
	gate := newCondStoreCompletionGate(t)
	s.highestGate = gate
	command := runCondStoreCompletionCommand(c, "fetch FETCH 1 (UID MODSEQ)")
	awaitCondStoreCompletion(t, gate.entered, "HIGHESTMODSEQ query")
	n := s.notify(10)
	awaitCondStoreCompletion(t, n.attempted, "notification attempt")
	// Without a shared response lock the producer finishes while the query
	// is gated. With one, it must wait for the tagged completion. A bounded
	// gate permits both valid serializations without making HighestModSeq
	// itself write a response or wait indefinitely for the response lock.
	select {
	case <-n.done:
	case <-time.After(time.Second):
	}
	gate.open()
	awaitCondStoreCompletionCommand(t, command)
	awaitCondStoreCompletion(t, n.done, "notification completion")
	if n.err != nil {
		t.Fatal(n.err)
	}
	response := out.String()
	tagIndex := strings.Index(response, "fetch OK ")
	if tagIndex < 0 {
		t.Fatalf("missing FETCH completion:\n%s", response)
	}
	if strings.Contains(response[:tagIndex], "MODSEQ (10)") {
		requireCondStoreCompletionWatermark(t, response, "fetch", 10)
		return
	}
	// A producer serialized after the tag starts the next span. Its MODSEQ
	// must survive any cleanup performed when the earlier command returns.
	awaitCondStoreCompletionCommand(t, runCondStoreCompletionCommand(c, "next FETCH 1 (UID MODSEQ)"))
	requireCondStoreCompletionWatermark(t, out.String()[tagIndex+len("fetch OK "):], "next", 10)
}

// ID writes its own tagged completion instead of using readCommand's normal
// OK path. That boundary still needs a compensating safe watermark after an
// unsolicited MODSEQ above a SELECTED-DELAYED expunge.
func TestCondStoreSelfTaggedIDWatermark(t *testing.T) {
	c, s, out := newCondStoreCompletionConn(t)
	gate := newCondStoreCompletionGate(t)
	s.idGate = gate
	command := runCondStoreCompletionCommand(c, "id ID NIL")
	awaitCondStoreCompletion(t, gate.entered, "ID backend")
	n := s.notify(10)
	awaitCondStoreCompletion(t, n.done, "ID notification")
	gate.open()
	awaitCondStoreCompletionCommand(t, command)
	if n.err != nil {
		t.Fatal(n.err)
	}
	requireCondStoreCompletionWatermark(t, out.String(), "id", 10)
}

func TestCondStoreModSeqAfterTagStartsNewSpan(t *testing.T) {
	c, s, out := newCondStoreCompletionConn(t)
	s.fetchModSeq = 10
	awaitCondStoreCompletionCommand(t, runCondStoreCompletionCommand(c, "first FETCH 1 (UID MODSEQ)"))
	requireCondStoreCompletionWatermark(t, out.String(), "first", 10)
	s.fetchModSeq = 2
	start := out.Len()
	awaitCondStoreCompletionCommand(t, runCondStoreCompletionCommand(c, "clean FETCH 1 (UID MODSEQ)"))
	if response := out.String()[start:]; strings.Contains(response, "[HIGHESTMODSEQ ") {
		t.Fatalf("previous tagged span leaked into the next command:\n%s", response)
	}
	start = out.Len()
	n := s.notify(11)
	awaitCondStoreCompletion(t, n.done, "between-command notification")
	if n.err != nil {
		t.Fatal(n.err)
	}
	awaitCondStoreCompletionCommand(t, runCondStoreCompletionCommand(c, "following FETCH 1 (UID MODSEQ)"))
	requireCondStoreCompletionWatermark(t, out.String()[start:], "following", 11)
}
