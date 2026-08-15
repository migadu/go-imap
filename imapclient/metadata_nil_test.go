package imapclient

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadMetadataRespNilValue covers an absent annotation value.
//
// RFC 5464 §4.2.1 uses NIL for an entry that has no value, and the caller has to
// be able to tell that apart from an entry whose value happens to be the text
// "NIL". NIL is an atom, so the generic atom branch of the value parser matched
// it first and handed back a three-byte value, leaving the ExpectNIL branch
// behind it unreachable.
func TestReadMetadataRespNilValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		wantNil bool
		want    string
	}{
		{name: "nil value", data: "\"INBOX\" (/private/comment NIL)\r\n", wantNil: true},
		{name: "quoted value", data: "\"INBOX\" (/private/comment \"hello\")\r\n", want: "hello"},
		{name: "quoted NIL is a value", data: "\"INBOX\" (/private/comment \"NIL\")\r\n", want: "NIL"},
		{name: "bare atom value", data: "\"INBOX\" (/private/comment hello)\r\n", want: "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.data)), imapwire.ConnSideClient)

			resp, err := readMetadataResp(dec, &Options{})
			if err != nil {
				t.Fatalf("readMetadataResp() = %v", err)
			}
			value, ok := resp.EntryValues["/private/comment"]
			if !ok {
				t.Fatalf("no /private/comment entry in %v", resp.EntryValues)
			}
			if tc.wantNil {
				if value != nil {
					t.Errorf("value = %q, want nil (the entry has no value)", string(*value))
				}
				return
			}
			if value == nil {
				t.Fatalf("value = nil, want %q", tc.want)
			}
			if got := string(*value); got != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
		})
	}
}
