package imapclient

import (
	"reflect"
	"testing"
)

func TestSortedNums(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    []uint32
		wantErr bool
	}{
		{in: "3,1,2", want: []uint32{3, 1, 2}},
		{in: "7", want: []uint32{7}},
		// RFC 5267 §3.2: a range means its numbers in increasing order, either way round.
		{in: "5,10:12,1", want: []uint32{5, 10, 11, 12, 1}},
		{in: "12:10,5", want: []uint32{10, 11, 12, 5}},
		{in: "1,2,", wantErr: true},
		{in: "1:*", wantErr: true},
		{in: "*", wantErr: true},
		{in: "0", wantErr: true},
		{in: "1:20000000", wantErr: true},
	} {
		got, err := sortedNums(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("sortedNums(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("sortedNums(%q): %v", tc.in, err)
		} else if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("sortedNums(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
