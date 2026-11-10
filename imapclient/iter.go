package imapclient

import (
	"iter"

	"github.com/migadu/go-imap"
)

// All returns an iterator over the messages of the FETCH command.
func (cmd *FetchCommand) All() iter.Seq[*FetchMessageData] {
	// TODO: not implemented
	return func(yield func(*FetchMessageData) bool) {}
}

// All returns an iterator over the data items of the message.
func (data *FetchMessageData) All() iter.Seq[FetchItemData] {
	// TODO: not implemented
	return func(yield func(FetchItemData) bool) {}
}

// All returns an iterator over the mailboxes of the LIST command.
func (cmd *ListCommand) All() iter.Seq[*imap.ListData] {
	// TODO: not implemented
	return func(yield func(*imap.ListData) bool) {}
}

// All returns an iterator over the sequence numbers of the expunged messages.
func (cmd *ExpungeCommand) All() iter.Seq[uint32] {
	// TODO: not implemented
	return func(yield func(uint32) bool) {}
}
