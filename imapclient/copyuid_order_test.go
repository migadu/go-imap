package imapclient

import (
	"bufio"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func TestCopyUIDOrderedRanges(t *testing.T) {
	for _, tc := range []struct {
		wire string
		want [][2]imap.UID
	}{
		{"7 2,1 101:102]", [][2]imap.UID{{2, 101}, {1, 102}}},
		{"7 4,1:3 101:102,110:111]", [][2]imap.UID{{4, 101}, {1, 102}, {2, 110}, {3, 111}}},
		{"7 12:10,2 103:101,110]", [][2]imap.UID{{10, 101}, {11, 102}, {12, 103}, {2, 110}}},
		// RFC 9051 section 7.1 normalizes range endpoints, but not list order.
		{"7 12:10 101:103]", [][2]imap.UID{{10, 101}, {11, 102}, {12, 103}}},
		{"7 12,11,10 101:103]", [][2]imap.UID{{12, 101}, {11, 102}, {10, 103}}},
		{"7 10:12 103:101]", [][2]imap.UID{{10, 101}, {11, 102}, {12, 103}}},
		{"7 4294967295 4294967295]", [][2]imap.UID{{4294967295, 4294967295}}},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.wire)), imapwire.ConnSideClient)
			data, err := readRespCodeCopyUID(dec)
			if err != nil || data.UIDValidity != 7 {
				t.Fatalf("data=%+v, err=%v", data, err)
			}
			var got [][2]imap.UID
			for source, dest := range data.UIDMapping.All() {
				got = append(got, [2]imap.UID{source, dest})
			}
			if !reflect.DeepEqual(got, tc.want) || !dec.Special(']') {
				t.Fatalf("pairs=%v; want %v; decoder error=%v", got, tc.want, dec.Err())
			}
		})
	}
}

func TestCopyUIDInvalidMappingsIgnored(t *testing.T) {
	for _, wire := range []string{
		"7 1:2 101]", "7 1 101:102]", "7 1,1 101:102]", "7 1:2 101,101]",
		"7 1:3,2:4 101:106]", "7 * 101]", "7 1:* 101:102]", "7 1 101,*]",
		"7 0 101]", "7 1 0]", "0 1 101]", "7 4294967296 101]", "7 1, 101]",
		"]", "7]", "7 1]", "7 1 ]", "7  101]", "nope 1 101]",
		"4294967296 1 101]", "7 1 101 extra]", "7 (1) 101]", "7 1 \"101\"]",
	} {
		t.Run(wire, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(wire)), imapwire.ConnSideClient)
			data, err := readRespCodeCopyUID(dec)
			if err != nil || data.UIDValidity != 0 || len(data.UIDMapping) != 0 || !dec.Special(']') {
				t.Fatalf("malformed advisory data not consumed and ignored: %+v, %v", data, err)
			}
		})
	}
}

func TestCopyUIDTolerancePreservesFramingAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wire    string
		maxSize int64
	}{
		{"missing closing bracket", "7 1 101\r\n* OK [COPYUID 8 2 102] next\r\n", 0},
		{"truncated code", "7 1 101", 0},
		{"size limit", "7 1 101" + strings.Repeat(" ", 100) + "]", 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.wire)), imapwire.ConnSideClient)
			dec.MaxSize = tc.maxSize
			_, err := readRespCodeCopyUID(dec)
			if err == nil && dec.ExpectSpecial(']') {
				t.Fatal("accepted broken framing or exceeded decoder budget")
			}
			if err == nil && dec.Err() == nil {
				t.Fatal("missing framing error")
			}
		})
	}
}

func TestCopyUIDHugeRangeRemainsCompact(t *testing.T) {
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("7 1:4294967295 1:4294967295]")), imapwire.ConnSideClient)
	data, err := readRespCodeCopyUID(dec)
	if err != nil || len(data.UIDMapping) != 1 || data.UIDMapping.Cardinality() != 4294967295 {
		t.Fatalf("large range expanded or truncated: %+v, %v", data, err)
	}
	count := 0
	for source, dest := range data.UIDMapping.All() {
		count++
		if source != imap.UID(count) || dest != source {
			t.Fatalf("incorrect prefix: %d -> %d", source, dest)
		}
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Fatal("iterator did not yield expected prefix")
	}
}
