package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Commands are answered in the order they were sent, so a sequence FETCH can
// only claim a response by number when no UID command precedes it. Here the
// UID FETCH was sent first and its response puts the body before the UID: it
// must still go to the UID FETCH, not to the later FETCH 1 (FLAGS), whose own
// response would then be dropped as a duplicate.
func TestUIDFetchBufferEarlierUIDFetchKeepsItsResponse(t *testing.T) {
	client := newUIDBufferLimitClient(t, nil, func(conn net.Conn, reader *bufio.Reader) error {
		var tags []string
		for i := 0; i < 2; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			tag, _, _ := strings.Cut(line, " ")
			tags = append(tags, tag)
		}
		_, err := fmt.Fprintf(conn, "* 1 FETCH (BODY[] {4}\r\ndata UID 9)\r\n%s OK uid fetch\r\n* 1 FETCH (FLAGS (\\Seen))\r\n%s OK fetch\r\n", tags[0], tags[1])
		return err
	})
	uidCmd := client.Fetch(imap.UIDSetNum(9), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{}}})
	seqCmd := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true})

	seqDone := make(chan []*imapclient.FetchMessageBuffer, 1)
	go func() {
		messages, err := seqCmd.Collect()
		if err != nil {
			t.Errorf("FETCH 1: %v", err)
		}
		seqDone <- messages
	}()
	uidMessages, err := uidCmd.Collect()
	if err != nil {
		t.Fatalf("UID FETCH 9: %v", err)
	}
	if len(uidMessages) != 1 || uidMessages[0].UID != 9 || string(uidMessages[0].FindBodySection(&imap.FetchItemBodySection{})) != "data" {
		t.Errorf("UID FETCH 9 returned %d messages, want UID 9 with body \"data\"", len(uidMessages))
	}
	select {
	case seqMessages := <-seqDone:
		if len(seqMessages) != 1 || len(seqMessages[0].Flags) != 1 || seqMessages[0].Flags[0] != imap.FlagSeen {
			t.Errorf("FETCH 1 returned %d messages, want one with \\Seen", len(seqMessages))
		} else if seqMessages[0].UID != 0 {
			t.Errorf("FETCH 1 got the UID FETCH's response (UID %d)", seqMessages[0].UID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FETCH 1 did not complete")
	}
}
