package imapclient

import (
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// A waiter must not observe SELECT completion while its selected-mailbox
// update is still waiting to acquire the client lock.
func TestCompleteCommandPublishesStateBeforeDone(t *testing.T) {
	client := &Client{state: imap.ConnStateAuthenticated}
	cmd := &SelectCommand{
		commandBase: commandBase{done: make(chan error, 1)},
		mailbox:     "INBOX",
		data:        imap.SelectData{ReadOnly: true, NumMessages: 1},
	}
	finished := make(chan struct{})
	client.mutex.Lock()
	go func() {
		client.completeCommand(cmd, nil)
		close(finished)
	}()
	select {
	case <-cmd.done:
		t.Error("SELECT completed before it could publish the selected mailbox")
	case <-time.After(50 * time.Millisecond):
	}
	client.mutex.Unlock()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("command completion did not finish after releasing the client lock")
	}
	if err := cmd.wait(); err != nil {
		t.Fatal(err)
	}
	if state := client.State(); state != imap.ConnStateSelected {
		t.Errorf("state after completion = %v, want selected", state)
	}
	if mailbox := client.Mailbox(); mailbox == nil || !mailbox.ReadOnly || mailbox.Name != "INBOX" || mailbox.NumMessages != 1 {
		t.Errorf("mailbox after completion = %+v, want selected read-only INBOX", mailbox)
	}
}
