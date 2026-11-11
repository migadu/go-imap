package imapclient

import (
	"iter"

	"github.com/migadu/go-imap"
)

// All returns an iterator over the messages of the FETCH command, for use
// with range. It is Next in a loop.
//
// The loop ends when there are no more messages, whether the command
// succeeded or not: the caller still calls Close, which tells. A loop left
// early leaves the remaining messages to Close.
func (cmd *FetchCommand) All() iter.Seq[*FetchMessageData] {
	return func(yield func(*FetchMessageData) bool) {
		for {
			msg := cmd.Next()
			if msg == nil || !yield(msg) {
				return
			}
		}
	}
}

// All returns an iterator over the data items of the message, for use with
// range. It is Next in a loop; an item is valid until the next one is asked
// for.
func (data *FetchMessageData) All() iter.Seq[FetchItemData] {
	return func(yield func(FetchItemData) bool) {
		for {
			item := data.Next()
			if item == nil || !yield(item) {
				return
			}
		}
	}
}

// All returns an iterator over the mailboxes of the LIST command, for use
// with range. It is Next in a loop; the caller still calls Close, as for
// FetchCommand.All.
func (cmd *ListCommand) All() iter.Seq[*imap.ListData] {
	return func(yield func(*imap.ListData) bool) {
		for {
			mbox := cmd.Next()
			if mbox == nil || !yield(mbox) {
				return
			}
		}
	}
}

// All returns an iterator over the sequence numbers of the expunged
// messages, for use with range. It is Next in a loop; the caller still calls
// Close, as for FetchCommand.All.
func (cmd *ExpungeCommand) All() iter.Seq[uint32] {
	return func(yield func(uint32) bool) {
		for {
			seqNum := cmd.Next()
			if seqNum == 0 || !yield(seqNum) {
				return
			}
		}
	}
}
