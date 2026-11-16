package imapserver_test

import (
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// Filtering must preserve the original bytes and ordering of retained fields,
// including duplicate names, unusual RFC 5322 field names and folded values.
func TestHeaderFieldFilteringPreservesRawFields(t *testing.T) {
	const subject = "sUbJeCt:  folded value\r\n\tcontinued value\r\n"
	const weirdFirst = "X-Weird(/@: first\r\n"
	const repeatFirst = "X-Repeat: one\r\n"
	const drop = "X-Drop: remove\r\n"
	const repeatSecond = "x-rEpEaT: two\r\n  folded duplicate\r\n"
	const weirdSecond = "x-wEiRd(/@: second\r\n"
	const nearMatch = "X-Weird(/@-Other: keep exact-name match\r\n"
	const last = "X-Trace: last\r\n"
	const headers = subject + weirdFirst + repeatFirst + drop + repeatSecond + weirdSecond + nearMatch + last
	const message = headers + "\r\nbody must not be returned\r\n"
	for _, tc := range []struct {
		name         string
		include, not []string
		want         string
	}{
		{"unfiltered", nil, nil, headers + "\r\n"},
		{"include_duplicates_and_unusual_names", []string{"SUBJECT", "x-repeat", "x-weird(/@"}, nil, subject + weirdFirst + repeatFirst + repeatSecond + weirdSecond + "\r\n"},
		{"exclude_unusual_case_variants", nil, []string{"X-WEIRD(/@", "x-drop"}, subject + repeatFirst + repeatSecond + nearMatch + last + "\r\n"},
		{"include_then_exclude", []string{"subject", "x-repeat", "x-weird(/@"}, []string{"X-REPEAT"}, subject + weirdFirst + weirdSecond + "\r\n"},
		{"include_absent", []string{"absent"}, nil, "\r\n"},
		{"exclude_absent", nil, []string{"absent"}, headers + "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, partial := range []*imap.SectionPartial{nil, {Offset: 7, Size: 31}, {Offset: int64(len(tc.want) + 10), Size: 3}} {
				item := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: tc.include, HeaderFieldsNot: tc.not, Partial: partial}
				want := tc.want
				if partial != nil {
					start := min(int(partial.Offset), len(want))
					end := min(start+int(partial.Size), len(want))
					want = want[start:end]
				}
				if got := string(imapserver.ExtractBodySection(strings.NewReader(message), item)); got != want {
					t.Errorf("partial %v: got %q, want original retained bytes %q", partial, got, want)
				}
			}
		})
	}
}

// Removing a repeated field must scale with the header block's size. Measure
// growth rather than an absolute duration, with four times the expected linear
// growth allowed for scheduling/GC noise. Best-of-three samples discard pauses
// without requiring a particular machine speed. The retained output is checked
// on every sample so avoiding the work by dropping the whole header cannot pass.
func TestHeaderFieldsNotRemovalScalesLinearly(t *testing.T) {
	const smallCount, largeCount = 10000, 80000
	const first = "sUbJeCt: preserve this\r\n\tand its folding\r\n"
	const last = "X-Keep: last\r\n"
	const want = first + last + "\r\n"
	for _, tc := range []struct {
		name, fieldName string
		fields          func(int) string
	}{
		{"duplicate_name", "x-drop", func(count int) string {
			return strings.Repeat("X-Drop: duplicate value\r\n", count)
		}},
		// All 80k names fold to one requested name but remain distinct raw
		// names. This prevents replacing per-field deletion with a quadratic
		// scan of the entire block for each observed case variant.
		{"distinct_non_token_case_variants", "x(abcdefghijklmnopq", func(count int) string {
			var fields strings.Builder
			for i := 0; i < count; i++ {
				name := []byte("X(abcdefghijklmnopq")
				for bit := 0; bit < 17; bit++ {
					if i&(1<<bit) != 0 {
						name[bit+2] -= 'a' - 'A'
					}
				}
				fields.Write(name)
				fields.WriteString(": value\r\n")
			}
			return fields.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := first + tc.fields(smallCount) + last + "\r\nbody\r\n"
			large := first + tc.fields(largeCount) + last + "\r\nbody\r\n"
			item := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFieldsNot: []string{tc.fieldName}}
			measure := func(message string) time.Duration {
				start := time.Now()
				got := imapserver.ExtractBodySection(strings.NewReader(message), item)
				elapsed := time.Since(start)
				if string(got) != want {
					t.Fatalf("header removal returned %q, want %q", got, want)
				}
				return elapsed
			}
			var smallBest, largeBest time.Duration
			for trial := 0; trial < 3; trial++ {
				for i, sample := range []string{small, large} {
					elapsed := measure(sample)
					best := &smallBest
					if i == 1 {
						best = &largeBest
					}
					if *best == 0 || elapsed < *best {
						*best = elapsed
					}
				}
			}
			growth := float64(largeBest) / float64(smallBest)
			t.Logf("removing headers: %d=%v, %d=%v, growth=%.2fx", smallCount, smallBest, largeCount, largeBest, growth)
			if largeBest > 32*smallBest {
				t.Fatalf("8x as many headers took %.2fx as long; want <=32x growth (generous linear-scaling bound)", growth)
			}
		})
	}
}
