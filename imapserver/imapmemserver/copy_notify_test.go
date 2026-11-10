package imapmemserver

import (
	"context"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// COPY cannot target the selected mailbox. It must neither record destination
// UIDs as self-created in that mailbox nor wait on its notification bookkeeping.
func TestCopyDoesNotLockSelectedNotificationState(t *testing.T) {
	u := NewUser("user", "password")
	for _, name := range []string{"INBOX", "Archive"} {
		if err := u.Create(context.Background(), name, nil); err != nil {
			t.Fatal(err)
		}
	}
	source := u.mailboxIfExists("INBOX")
	for range 3 {
		source.appendBytes([]byte("Subject: copy\r\n\r\nbody"), &imap.AppendOptions{})
	}
	sess := NewUserSession(u)
	t.Cleanup(func() { sess.Close() })
	if _, err := sess.Select(context.Background(), "INBOX", &imap.SelectOptions{}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		data *imap.CopyData
		err  error
	}
	done := make(chan result, 1)
	sess.notifyMutex.Lock()
	go func() {
		data, err := sess.Copy(context.Background(), imap.UIDSetNum(1, 2, 3), "Archive")
		done <- result{data, err}
	}()
	var got result
	select {
	case got = <-done:
		sess.notifyMutex.Unlock()
	case <-time.After(time.Second):
		sess.notifyMutex.Unlock()
		<-done
		t.Fatal("COPY waited on notification state for an unrelated selected mailbox")
	}
	if got.err != nil || got.data == nil || got.data.UIDMapping.Cardinality() != 3 {
		t.Fatalf("COPY = %+v, %v", got.data, got.err)
	}
	if len(sess.ownMessages) != 0 {
		t.Fatalf("COPY recorded destination UIDs in selected mailbox: %v", sess.ownMessages)
	}
	for _, name := range []string{"INBOX", "Archive"} {
		status := u.mailboxIfExists(name).StatusData(&imap.StatusOptions{NumMessages: true})
		if status.NumMessages == nil || *status.NumMessages != 3 {
			t.Fatalf("%s message count = %v, want 3", name, status.NumMessages)
		}
	}
}
