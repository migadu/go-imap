package imap

import "testing"

func TestModifiedResponseCode(t *testing.T) {
	tests := []struct {
		name   string
		numSet NumSet
		want   ResponseCode
	}{
		{
			name:   "seq set",
			numSet: SeqSetNum(7, 9),
			want:   "MODIFIED 7,9",
		},
		{
			name:   "uid set",
			numSet: UIDSetNum(7, 9),
			want:   "MODIFIED 7,9",
		},
		{
			name:   "contiguous set is collapsed into a range",
			numSet: SeqSetNum(1, 2, 3),
			want:   "MODIFIED 1:3",
		},
		{
			// A conditional STORE in which every message satisfied the
			// precondition must not report MODIFIED at all: a bare "[MODIFIED]"
			// is not in the grammar, so an empty set yields no response code.
			name:   "empty seq set",
			numSet: SeqSet(nil),
			want:   "",
		},
		{
			name:   "empty uid set",
			numSet: UIDSet(nil),
			want:   "",
		},
		{
			name:   "nil",
			numSet: nil,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ModifiedResponseCode(tt.numSet); got != tt.want {
				t.Errorf("ModifiedResponseCode(%v) = %q, want %q", tt.numSet, got, tt.want)
			}
		})
	}
}
