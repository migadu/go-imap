package imapclient_test

import (
	"testing"

	"github.com/migadu/go-imap"
)

// A section the server sends as a zero-length literal is present and empty.
// FindBodySection and FindBinarySection return nil only for a section the
// response does not carry, so they must not return nil for this one.
func TestFetchFindEmptySection(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		client := newSelectedCodeServer(t, "* 1 FETCH (BODY[] {0}\r\n)\r\n")
		section := &imap.FetchItemBodySection{}
		msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{section},
		}).Collect()
		if err != nil {
			t.Fatalf("Fetch().Collect() = %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if b := msgs[0].FindBodySection(section); b == nil || len(b) != 0 {
			t.Errorf("FindBodySection() = %#v, want an empty non-nil slice", b)
		}
		if b := msgs[0].FindBodySection(&imap.FetchItemBodySection{Part: []int{1}}); b != nil {
			t.Errorf("FindBodySection(absent) = %#v, want nil", b)
		}
	})
	t.Run("binary", func(t *testing.T) {
		client := newSelectedCodeServer(t, "* 1 FETCH (BINARY[] ~{0}\r\n)\r\n")
		section := &imap.FetchItemBinarySection{}
		msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
			BinarySection: []*imap.FetchItemBinarySection{section},
		}).Collect()
		if err != nil {
			t.Fatalf("Fetch().Collect() = %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if b := msgs[0].FindBinarySection(section); b == nil || len(b) != 0 {
			t.Errorf("FindBinarySection() = %#v, want an empty non-nil slice", b)
		}
		if b := msgs[0].FindBinarySection(&imap.FetchItemBinarySection{Part: []int{1}}); b != nil {
			t.Errorf("FindBinarySection(absent) = %#v, want nil", b)
		}
	})
}
