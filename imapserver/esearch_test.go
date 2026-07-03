package imapserver

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestMultiSearchDataIsEmpty locks in RFC 7377 §2.1: an ESEARCH response MUST
// NOT be returned for a mailbox with no matching messages, regardless of the
// requested return options (including COUNT 0).
func TestMultiSearchDataIsEmpty(t *testing.T) {
	nonEmpty := imap.UIDSet{{Start: 1, Stop: 3}}

	tests := []struct {
		name    string
		data    imap.SearchData
		options imap.SearchOptions
		want    bool
	}{
		{
			name:    "count zero is empty (MUST NOT emit)",
			data:    imap.SearchData{Count: 0},
			options: imap.SearchOptions{ReturnCount: true},
			want:    true,
		},
		{
			name:    "count non-zero is not empty",
			data:    imap.SearchData{Count: 5},
			options: imap.SearchOptions{ReturnCount: true},
			want:    false,
		},
		{
			name:    "all empty is empty",
			data:    imap.SearchData{All: imap.UIDSet{}},
			options: imap.SearchOptions{ReturnAll: true},
			want:    true,
		},
		{
			name:    "all nil is empty",
			data:    imap.SearchData{All: nil},
			options: imap.SearchOptions{ReturnAll: true},
			want:    true,
		},
		{
			name:    "all non-empty is not empty",
			data:    imap.SearchData{All: nonEmpty},
			options: imap.SearchOptions{ReturnAll: true},
			want:    false,
		},
		{
			name:    "min hit is not empty",
			data:    imap.SearchData{Min: 2},
			options: imap.SearchOptions{ReturnMin: true},
			want:    false,
		},
		{
			name:    "min zero (no hit) is empty",
			data:    imap.SearchData{Min: 0},
			options: imap.SearchOptions{ReturnMin: true},
			want:    true,
		},
		{
			name:    "max hit is not empty",
			data:    imap.SearchData{Max: 9},
			options: imap.SearchOptions{ReturnMax: true},
			want:    false,
		},
		{
			name:    "min+max both zero is empty",
			data:    imap.SearchData{Min: 0, Max: 0},
			options: imap.SearchOptions{ReturnMin: true, ReturnMax: true},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := multiSearchDataIsEmpty(&tt.data, &tt.options); got != tt.want {
				t.Errorf("multiSearchDataIsEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}
