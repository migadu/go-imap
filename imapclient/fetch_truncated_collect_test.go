package imapclient_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// A FETCH response the client could not finish reading must not be collected
// as if it were whole. A message is handed to its consumer as soon as it
// carries a literal, so a failure further into the response reaches a message
// already being read. FetchMessageData.Collect used to return a nil error for
// it: the items read before the failure were there, the rest were silently
// missing, and only FetchCommand.Close told the two cases apart.
//
// The reported case is a server sending a BINARY.SIZE that does not fit the
// 32 bits RFC 9051 Section 9 gives a number ("BINARY.SIZE" section-binary SP
// number). The decoder also described that failure as
//
//	imapwire: expected number, got ")"
//
// which points at the byte after the number rather than at the number.
//
// Upstream report: emersion/go-imap#630 by agcom.
func TestFetchCollectTruncatedMessage(t *testing.T) {
	client := newCodeServer(t, "* 1 FETCH (UID 63 BODY[] {5}\r\nhello BINARY.SIZE[1] 18446744073709551232)\r\n")

	cmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		UID:               true,
		BodySection:       []*imap.FetchItemBodySection{{}},
		BinarySectionSize: []*imap.FetchItemBinarySectionSize{{Part: []int{1}}},
	})
	defer cmd.Close()

	msg := cmd.Next()
	if msg == nil {
		t.Fatalf("Next() = nil, want the message (Close() = %v)", cmd.Close())
	}
	buf, err := msg.Collect()
	if err == nil {
		t.Fatalf("Collect() = %+v, nil; want an error for a message cut short", buf)
	}
	if !strings.Contains(err.Error(), "number too large") {
		t.Errorf("Collect() = %q, want it to name the oversized number", err)
	}
	if closeErr := cmd.Close(); closeErr == nil {
		t.Errorf("Close() = nil, want the error that cut the response short")
	}
}
