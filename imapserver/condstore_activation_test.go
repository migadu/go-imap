package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// Block only after a complete, validated enabling SEARCH reaches the backend.
// A flag notification sent meanwhile must already include MODSEQ, even though
// the SEARCH's successful tagged response has not yet been written.
type acceptedCondStoreSearchSession struct {
	*midParseNotifySession
	entered chan struct{}
	release chan struct{}
}

func (s *acceptedCondStoreSearchSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Session.Search(ctx, kind, criteria, options)
}

func (s *acceptedCondStoreSearchSession) HighestModSeq() uint64 { return firstEnableHighestModSeq }

func TestAcceptedEnablingCommandIncludesConcurrentNotifyModSeq(t *testing.T) {
	sessions := make(chan *acceptedCondStoreSearchSession, 1)
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}, imap.CapNotify: {}}, func(backend imapserver.Session) imapserver.Session {
		s := &acceptedCondStoreSearchSession{
			midParseNotifySession: &midParseNotifySession{Session: backend, trigger: make(chan struct{}), written: make(chan struct{})},
			entered:               make(chan struct{}), release: make(chan struct{}),
		}
		sessions <- s
		return s
	})
	s := <-sessions
	rc.appendOne("test")
	rc.do("SELECT INBOX")
	rc.do("NOTIFY SET (SELECTED (MessageNew (UID FLAGS) MessageExpunge FlagChange))")
	if _, err := fmt.Fprint(rc.c, "running SEARCH MODSEQ 1\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted SEARCH did not reach backend")
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(s.release) }) }
	defer unblock()
	close(s.trigger)
	select {
	case <-s.written:
	case <-time.After(5 * time.Second):
		t.Fatal("NOTIFY did not write flag change")
	}
	line, err := rc.br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	completion := (&storeTestConn{t: t, conn: rc.c, br: rc.br}).readUntilTag("running")
	if !strings.Contains(completion, "running OK") {
		t.Fatalf("accepted SEARCH did not finish OK: %q", completion)
	}
	if !strings.Contains(line, "FETCH") {
		t.Fatalf("want unsolicited FETCH during accepted SEARCH, got %q", line)
	}
	if !strings.Contains(line, "MODSEQ (") {
		t.Fatalf("valid fully parsed enabling SEARCH already executing, but concurrent flag FETCH omits MODSEQ: %q", line)
	}
}
