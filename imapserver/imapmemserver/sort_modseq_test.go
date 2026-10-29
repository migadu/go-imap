package imapmemserver

import (
	"context"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// The wire handler turns a SORT without return options into ALL, but a direct
// caller may pass empty options. They mean ALL, as Search treats them, so
// MODSEQ covers every match instead of none.
func TestSortEmptyOptionsModSeq(t *testing.T) {
	mbox := NewMailbox("INBOX", 1)
	mbox.appendBytes([]byte("Subject: a\r\n\r\na"), &imap.AppendOptions{})
	mbox.appendBytes([]byte("Subject: b\r\n\r\nb"), &imap.AppendOptions{})
	view := mbox.NewView()
	defer view.Close()

	for _, tc := range []struct {
		name    string
		options *imap.SortOptions
	}{
		{"nil", nil},
		{"empty", &imap.SortOptions{}},
	} {
		data, err := view.Sort(context.Background(), imapserver.NumKindSeq, []imap.SortCriterion{{Key: imap.SortKeyArrival}}, "UTF-8", &imap.SearchCriteria{}, tc.options)
		if err != nil {
			t.Fatalf("%s options: Sort() = %v", tc.name, err)
		}
		if data.ModSeq != mbox.highestModSeq {
			t.Errorf("%s options: ModSeq = %v, want %v", tc.name, data.ModSeq, mbox.highestModSeq)
		}
	}
}
