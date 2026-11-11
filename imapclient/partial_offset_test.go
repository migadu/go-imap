package imapclient_test

import (
	"testing"

	"github.com/migadu/go-imap"
)

// A server answering a number64 partial request echoes a number64 origin
// octet (RFC 9051 §6.4.5 lets the request carry one). The client must read
// it rather than fail the whole FETCH on a number wider than 32 bits.
func TestFetchPartialOffsetWiderThan32Bits(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp string
	}{
		{"body", "* 1 FETCH (BODY[]<4294967301> \"\")\r\n"},
		{"binary", "* 1 FETCH (BINARY[]<4294967301> \"\")\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSelectedCodeServer(t, tc.resp)
			opts := &imap.FetchOptions{}
			const offset = 1<<32 + 5
			if tc.name == "body" {
				opts.BodySection = []*imap.FetchItemBodySection{{Partial: &imap.SectionPartial{Offset: offset, Size: 10}}}
			} else {
				opts.BinarySection = []*imap.FetchItemBinarySection{{Partial: &imap.SectionPartial{Offset: offset, Size: 10}}}
			}
			msgs, err := client.Fetch(imap.SeqSetNum(1), opts).Collect()
			if err != nil {
				t.Fatalf("Fetch().Collect() = %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			var got int64 = -1
			if tc.name == "body" {
				for _, s := range msgs[0].BodySection {
					if s.Section.Partial != nil {
						got = s.Section.Partial.Offset
					}
				}
			} else {
				for _, s := range msgs[0].BinarySection {
					if s.Section.Partial != nil {
						got = s.Section.Partial.Offset
					}
				}
			}
			if got != offset {
				t.Errorf("parsed origin octet %d, want %d", got, offset)
			}
		})
	}
}
