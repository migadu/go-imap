package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestFetchExtraSP(t *testing.T) {
	for _, tc := range []struct {
		name, attrs string
		seen        bool
	}{
		// The UID-only response is sent by imap.qq.com (alps issue #967).
		{"QQ trailing space", "UID 15557 ", false},
		{"leading spaces", "  UID 15557", false},
		{"multiple trailing spaces", "UID 15557   ", false},
		{"multiple separators", "UID 15557  FLAGS (\\Seen)", true},
		{"spaces throughout", "  UID 15557  FLAGS (\\Seen)  ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFetchExtraSPClient(t, tc.attrs)
			messages, err := client.Fetch(imap.UIDSetNum(15557), &imap.FetchOptions{UID: true, Flags: tc.seen}).Collect()
			if err != nil || len(messages) != 1 {
				t.Fatalf("Collect() returned %d messages, %v", len(messages), err)
			}
			if messages[0].UID != 15557 || messages[0].SeqNum != 261 {
				t.Errorf("message = %+v, want UID 15557 and sequence number 261", messages[0])
			}
			if tc.seen && (len(messages[0].Flags) != 1 || messages[0].Flags[0] != imap.FlagSeen) {
				t.Errorf("Flags = %v, want [\\Seen]", messages[0].Flags)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("NOOP after FETCH: %v", err)
			}
		})
	}
}

func TestFetchExtraSPPreservesLiteral(t *testing.T) {
	const payload = "  ) UID 999\r\ntext )  "
	for _, uidFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("UID first=%v", uidFirst), func(t *testing.T) {
			literal := fmt.Sprintf("BODY[] {%d}\r\n%s", len(payload), payload)
			attrs := "  " + literal + "  UID 15557  "
			if uidFirst {
				attrs = "  UID 15557  " + literal + "  "
			}
			client := newFetchExtraSPClient(t, attrs)
			section := &imap.FetchItemBodySection{}
			messages, err := client.Fetch(imap.UIDSetNum(15557), &imap.FetchOptions{
				UID: true, BodySection: []*imap.FetchItemBodySection{section},
			}).Collect()
			if err != nil || len(messages) != 1 {
				t.Fatalf("Collect() returned %d messages, %v", len(messages), err)
			}
			if messages[0].UID != 15557 {
				t.Errorf("UID = %v, want 15557", messages[0].UID)
			}
			if got := string(messages[0].FindBodySection(section)); got != payload {
				t.Errorf("body = %q, want %q", got, payload)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("NOOP after FETCH: %v", err)
			}
		})
	}
}

func TestFetchExtraSPRejectsOtherSeparators(t *testing.T) {
	for _, tc := range []struct{ name, attrs string }{
		{"tab", "UID 15557\t"},
		{"nonempty literal without separator", "BODY[] {1}\r\nxUID 15557"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSelectedCodeServer(t, "* 261 FETCH ("+tc.attrs+")\r\n")
			_, err := client.Fetch(imap.UIDSetNum(15557), &imap.FetchOptions{
				UID: true, BodySection: []*imap.FetchItemBodySection{{}},
			}).Collect()
			if err == nil || !strings.Contains(err.Error(), "imapwire: expected SP") {
				t.Fatalf("Collect() = %v, want a separator error", err)
			}
		})
	}
}

// Answer FETCH once, then NOOP, so each success also checks response framing
// and that parsing the extra spaces leaves the connection usable.
func newFetchExtraSPClient(t *testing.T, attrs string) *imapclient.Client {
	return newUIDBufferLimitClient(t, nil, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "* 261 FETCH (%s)\r\n%s OK fetched\r\n", attrs, tag); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != "NOOP" {
			return fmt.Errorf("expected NOOP, got %q", line)
		}
		_, err = fmt.Fprintf(conn, "%s OK noop\r\n", fields[0])
		return err
	})
}
