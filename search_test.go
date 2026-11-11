package imap

import (
	"testing"
	"time"
)

// And intersects (RFC 9051 §6.4.4: "the result is the intersection (AND
// function) of all the messages that match those keys"), so a size limit on
// either operand holds for the result. A zero Larger or Smaller means the
// operand has no such limit, not a limit of zero octets, and the tighter one
// wins when both have it.
func TestSearchCriteriaAndSizeLimits(t *testing.T) {
	since := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		criteria, other SearchCriteria
		larger, smaller int64
	}{
		{name: "SMALLER and no size limit", criteria: SearchCriteria{Smaller: 100}, other: SearchCriteria{Since: since}, smaller: 100},
		{name: "SMALLER and LARGER", criteria: SearchCriteria{Smaller: 100}, other: SearchCriteria{Larger: 10}, larger: 10, smaller: 100},
		{name: "both and no size limit", criteria: SearchCriteria{Larger: 10, Smaller: 100}, other: SearchCriteria{Since: since}, larger: 10, smaller: 100},
		{name: "no size limit and SMALLER", criteria: SearchCriteria{Since: since}, other: SearchCriteria{Smaller: 100}, smaller: 100},
		{name: "SMALLER and a tighter SMALLER", criteria: SearchCriteria{Smaller: 200}, other: SearchCriteria{Smaller: 100}, smaller: 100},
		{name: "SMALLER and a looser SMALLER", criteria: SearchCriteria{Smaller: 100}, other: SearchCriteria{Smaller: 200}, smaller: 100},
		{name: "LARGER and no size limit", criteria: SearchCriteria{Larger: 10}, other: SearchCriteria{Since: since}, larger: 10},
		{name: "LARGER and SMALLER", criteria: SearchCriteria{Larger: 10}, other: SearchCriteria{Smaller: 100}, larger: 10, smaller: 100},
		{name: "no size limit and LARGER", criteria: SearchCriteria{Since: since}, other: SearchCriteria{Larger: 10}, larger: 10},
		{name: "LARGER and a tighter LARGER", criteria: SearchCriteria{Larger: 10}, other: SearchCriteria{Larger: 20}, larger: 20},
		{name: "LARGER and a looser LARGER", criteria: SearchCriteria{Larger: 20}, other: SearchCriteria{Larger: 10}, larger: 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.criteria.And(&tc.other)
			if tc.criteria.Larger != tc.larger {
				t.Errorf("Larger = %v, want %v", tc.criteria.Larger, tc.larger)
			}
			if tc.criteria.Smaller != tc.smaller {
				t.Errorf("Smaller = %v, want %v", tc.criteria.Smaller, tc.smaller)
			}
		})
	}
}

// YOUNGER and OLDER intersect like every other key (RFC 9051 §6.4.4): of two
// YOUNGER intervals the shorter holds, of two OLDER intervals the longer. Zero
// means the operand has no such limit (RFC 5032 §3 makes the interval an
// nz-number).
func TestSearchCriteriaAndWithin(t *testing.T) {
	for _, tc := range []struct {
		name            string
		criteria, other SearchCriteria
		younger, older  uint32
	}{
		{name: "YOUNGER and no limit", criteria: SearchCriteria{Younger: 60}, other: SearchCriteria{Larger: 1}, younger: 60},
		{name: "no limit and YOUNGER", criteria: SearchCriteria{Larger: 1}, other: SearchCriteria{Younger: 60}, younger: 60},
		{name: "YOUNGER and a shorter YOUNGER", criteria: SearchCriteria{Younger: 60}, other: SearchCriteria{Younger: 30}, younger: 30},
		{name: "YOUNGER and a longer YOUNGER", criteria: SearchCriteria{Younger: 30}, other: SearchCriteria{Younger: 60}, younger: 30},
		{name: "OLDER and no limit", criteria: SearchCriteria{Older: 60}, other: SearchCriteria{Larger: 1}, older: 60},
		{name: "no limit and OLDER", criteria: SearchCriteria{Larger: 1}, other: SearchCriteria{Older: 60}, older: 60},
		{name: "OLDER and a shorter OLDER", criteria: SearchCriteria{Older: 60}, other: SearchCriteria{Older: 30}, older: 60},
		{name: "OLDER and a longer OLDER", criteria: SearchCriteria{Older: 30}, other: SearchCriteria{Older: 60}, older: 60},
		{name: "YOUNGER and OLDER", criteria: SearchCriteria{Younger: 60}, other: SearchCriteria{Older: 30}, younger: 60, older: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.criteria.And(&tc.other)
			if tc.criteria.Younger != tc.younger {
				t.Errorf("Younger = %v, want %v", tc.criteria.Younger, tc.younger)
			}
			if tc.criteria.Older != tc.older {
				t.Errorf("Older = %v, want %v", tc.criteria.Older, tc.older)
			}
		})
	}
}
