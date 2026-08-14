package imapmemserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// TestStoreModifiedConcurrentExpunge stresses the window between a conditional
// STORE selecting its failed messages and building the MODIFIED response code.
// The MODIFIED set must be encoded while mbox.mutex is still held: encoding it
// afterwards lets a concurrent expunge queue into the session tracker in
// between, silently dropping entries — up to Store returning nil (a plain
// tagged OK) for a conditional store that stored nothing.
//
// Here message 2 always fails the UNCHANGEDSINCE 0 probe, so Store must report
// MODIFIED on every trial, no matter how an expunge of message 1 interleaves.
func TestStoreModifiedConcurrentExpunge(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)

	for trial := 0; trial < 20000 && time.Now().Before(deadline); trial++ {
		mbox := NewMailbox("INBOX", 1)
		// Message 1 is flagged \Deleted so the racing EXPUNGE removes it.
		mbox.appendBytes([]byte("a"), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDeleted}})
		mbox.appendBytes([]byte("b"), &imap.AppendOptions{})

		view := mbox.NewView()

		done := make(chan struct{})
		go func() {
			defer close(done)
			mbox.Expunge(ctx, nil, nil)
		}()

		// The always-fail probe on message 2: it exists on both sides of the
		// expunge (as sequence number 2 for this session, which has not been
		// told about the expunge), so its failure must be reported.
		err := view.Store(ctx, nil, imap.SeqSetNum(2), &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagSeen},
		}, &imap.StoreOptions{UnchangedSinceSet: true})

		<-done
		view.Close()

		if err == nil {
			t.Fatalf("trial %v: Store returned nil (plain OK) for a conditional store that stored nothing", trial)
		}
		var imapErr *imap.Error
		if !errors.As(err, &imapErr) {
			t.Fatalf("trial %v: Store returned %v, want *imap.Error carrying MODIFIED", trial, err)
		}
		if code := string(imapErr.Code); code != "MODIFIED 2" {
			if !strings.HasPrefix(code, "MODIFIED") {
				t.Fatalf("trial %v: response code = %q, want MODIFIED", trial, code)
			}
			t.Fatalf("trial %v: response code = %q, want %q", trial, code, "MODIFIED 2")
		}
	}
}
