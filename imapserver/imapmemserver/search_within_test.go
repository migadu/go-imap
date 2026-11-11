package imapmemserver

import (
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

func TestMatchWithinBoundaries(t *testing.T) {
	now := time.Date(2020, 1, 1, 0, 0, 0, 750000000, time.UTC)
	for _, tc := range []struct {
		name           string
		time           time.Time
		younger, older uint32
		want           bool
	}{
		{"unset", time.Time{}, 0, 0, true},
		{"younger at target", now.Add(-60 * time.Second), 60, 0, true},
		{"younger before target", now.Add(-61 * time.Second), 60, 0, false},
		{"older at target", now.Add(-60 * time.Second), 0, 60, true},
		{"older after target", now.Add(-59 * time.Second), 0, 60, false},
		{"both at target", now.Add(-60 * time.Second), 60, 60, true},
		{"maximum interval", now.Add(-time.Duration(^uint32(0)) * time.Second), ^uint32(0), ^uint32(0), true},
		{"same second", now.Truncate(time.Second).Add(-60 * time.Second), 60, 60, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchWithin(tc.time, now, tc.younger, tc.older); got != tc.want {
				t.Fatalf("matchWithin() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSearchWithinNestedReferenceTime(t *testing.T) {
	// The reference time deliberately differs from the wall clock. Nested
	// keys must share it, or a command crossing a second can satisfy the
	// contradiction YOUNGER 60 NOT YOUNGER 60.
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	msg := &message{t: now.Add(-60 * time.Second)}
	for _, tc := range []struct {
		name     string
		criteria imap.SearchCriteria
		want     bool
	}{
		{"contradiction", imap.SearchCriteria{Younger: 60, Not: []imap.SearchCriteria{{Younger: 60}}}, false},
		{"nested not", imap.SearchCriteria{Not: []imap.SearchCriteria{{Older: 61}}}, true},
		{"nested or", imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{{Older: 61}, {Younger: 60}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := msg.search(1, &tc.criteria, now); got != tc.want {
				t.Fatalf("search() = %v, want %v", got, tc.want)
			}
		})
	}
}
