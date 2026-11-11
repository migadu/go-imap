package imapclient_test

import (
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// Upstream proposal: emersion/go-imap#634. Issue #141.
func TestFetchCommandAll(t *testing.T) {
	client := newSelectedCodeServer(t, "* 1 FETCH (UID 11 FLAGS (\\Seen))\r\n* 2 FETCH (UID 12 FLAGS ())\r\n")

	cmd := client.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{UID: true, Flags: true})
	var seqNums []uint32
	var uids []imap.UID
	for msg := range cmd.All() {
		seqNums = append(seqNums, msg.SeqNum)
		for item := range msg.All() {
			if item, ok := item.(imapclient.FetchItemDataUID); ok {
				uids = append(uids, item.UID)
			}
		}
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if !slices.Equal(seqNums, []uint32{1, 2}) || !slices.Equal(uids, []imap.UID{11, 12}) {
		t.Errorf("iterated over messages %v with UIDs %v, want [1 2] and [11 12]", seqNums, uids)
	}
}

// Leaving the loop early leaves the rest to Close, as an unfinished Next loop
// does, and the connection stays usable.
func TestFetchCommandAllBreak(t *testing.T) {
	client := newSelectedCodeServer(t, "* 1 FETCH (UID 11)\r\n* 2 FETCH (UID 12)\r\n")

	cmd := client.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{UID: true})
	var n int
	for range cmd.All() {
		n++
		break
	}
	if n != 1 {
		t.Errorf("the loop ran %d times before break, want 1", n)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := client.Noop().Wait(); err != nil {
		t.Errorf("Noop() after a loop left early = %v", err)
	}
}

// The loop ends when the data ends, for whatever reason; Close tells why.
func TestFetchCommandAllError(t *testing.T) {
	client := newSelectedCodeServer(t, "* 1 FETCH (UID 11)\r\n* 2 FETCH (UID twelve)\r\n")

	cmd := client.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{UID: true})
	var seqNums []uint32
	for msg := range cmd.All() {
		seqNums = append(seqNums, msg.SeqNum)
	}
	if !slices.Equal(seqNums, []uint32{1}) {
		t.Errorf("iterated over messages %v, want [1]", seqNums)
	}
	if err := cmd.Close(); err == nil {
		t.Errorf("Close() = nil, want the error that ended the response")
	}
}

func TestListCommandAll(t *testing.T) {
	client := newCodeServer(t, "* LIST () \"/\" INBOX\r\n* LIST (\\Noselect) \"/\" Archive\r\n")

	cmd := client.List("", "*", nil)
	var names []string
	for mbox := range cmd.All() {
		names = append(names, mbox.Mailbox)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if !slices.Equal(names, []string{"INBOX", "Archive"}) {
		t.Errorf("iterated over %v, want [INBOX Archive]", names)
	}
}

func TestExpungeCommandAll(t *testing.T) {
	client := newSelectedCodeServer(t, "* 3 EXPUNGE\r\n* 3 EXPUNGE\r\n* 5 EXPUNGE\r\n")

	cmd := client.Expunge()
	var seqNums []uint32
	for seqNum := range cmd.All() {
		seqNums = append(seqNums, seqNum)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if !slices.Equal(seqNums, []uint32{3, 3, 5}) {
		t.Errorf("iterated over %v, want [3 3 5]", seqNums)
	}
}
