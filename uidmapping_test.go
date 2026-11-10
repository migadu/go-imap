package imap_test

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestUIDMappingPreservesPairs(t *testing.T) {
	want := [][2]imap.UID{{3, 101}, {1, 102}, {2, 103}, {4294967295, 4294967295}}
	var mapping imap.UIDMapping
	for _, pair := range want {
		mapping.Add(pair[0], pair[1])
	}
	if err := mapping.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 3 || mapping.Cardinality() != 4 {
		t.Fatalf("consecutive pairs not compressed: %+v", mapping)
	}
	var got [][2]imap.UID
	for source, dest := range mapping.All() {
		got = append(got, [2]imap.UID{source, dest})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs=%v, want=%v", got, want)
	}
}

func TestUIDMappingValidation(t *testing.T) {
	for _, mapping := range []imap.UIDMapping{
		nil,
		{{Source: imap.UIDRange{0, 0}, Dest: imap.UIDRange{1, 1}}},
		{{Source: imap.UIDRange{2, 1}, Dest: imap.UIDRange{1, 2}}},
		{{Source: imap.UIDRange{1, 2}, Dest: imap.UIDRange{1, 1}}},
		{{Source: imap.UIDRange{1, 1}, Dest: imap.UIDRange{0, 0}}},
		{{Source: imap.UIDRange{1, 2}, Dest: imap.UIDRange{10, 11}}, {Source: imap.UIDRange{2, 3}, Dest: imap.UIDRange{12, 13}}},
		{{Source: imap.UIDRange{1, 2}, Dest: imap.UIDRange{10, 11}}, {Source: imap.UIDRange{3, 4}, Dest: imap.UIDRange{11, 12}}},
	} {
		if err := mapping.Validate(); err == nil {
			t.Errorf("accepted invalid mapping: %+v", mapping)
		}
	}
}

// Public range literals can bypass Add and Validate. Accessors must not wrap
// arithmetic, yield zero UIDs, or expose a prefix of a malformed mapping.
func TestUIDMappingMalformedRangeAccessors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mapping imap.UIDMapping
	}{
		{"reversed source", imap.UIDMapping{{Source: imap.UIDRange{4, 1}, Dest: imap.UIDRange{10, 13}}}},
		{"reversed destination", imap.UIDMapping{{Source: imap.UIDRange{1, 4}, Dest: imap.UIDRange{13, 10}}}},
		{"zero source", imap.UIDMapping{{Source: imap.UIDRange{0, 1}, Dest: imap.UIDRange{10, 11}}}},
		{"zero destination", imap.UIDMapping{{Source: imap.UIDRange{1, 2}, Dest: imap.UIDRange{0, 1}}}},
		{"short destination", imap.UIDMapping{{Source: imap.UIDRange{1, 3}, Dest: imap.UIDRange{10, 11}}}},
		{"long destination", imap.UIDMapping{{Source: imap.UIDRange{1, 2}, Dest: imap.UIDRange{10, 12}}}},
		{"destination overflow", imap.UIDMapping{{Source: imap.UIDRange{1, 2}, Dest: imap.UIDRange{4294967295, 4294967295}}}},
		{"invalid suffix", imap.UIDMapping{
			{Source: imap.UIDRange{1, 1}, Dest: imap.UIDRange{10, 10}},
			{Source: imap.UIDRange{4, 1}, Dest: imap.UIDRange{11, 14}},
		}},
		{"exceeds UID space", imap.UIDMapping{
			{Source: imap.UIDRange{1, 4294967295}, Dest: imap.UIDRange{1, 4294967295}},
			{Source: imap.UIDRange{1, 1}, Dest: imap.UIDRange{1, 1}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if count := tc.mapping.Cardinality(); count != 0 {
				t.Errorf("malformed mapping cardinality = %d, want 0", count)
			}
			for source, dest := range tc.mapping.All() {
				t.Errorf("malformed mapping yielded %d -> %d", source, dest)
				break // A broken implementation must not expand a huge range.
			}
		})
	}
}
