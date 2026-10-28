package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// A UID FETCH can claim a message only by its UID, and a server may send the
// UID after an empty body: Open-Xchange does, without the SP between them
// (https://github.com/emersion/go-imap/issues/774). Such a message must reach
// the command, not vanish.
func TestUIDFetchEmptyBodyBeforeUID(t *testing.T) {
	headerRefs := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"References"},
		Peek:         true,
	}
	for _, tc := range []struct {
		name     string
		section  *imap.FetchItemBodySection
		response string
	}{
		{"literal, no SP", &imap.FetchItemBodySection{}, "* 1 FETCH (BODY[] {0}\r\nUID 42)"},
		{"literal", &imap.FetchItemBodySection{}, "* 1 FETCH (BODY[] {0}\r\n UID 42)"},
		{"quoted", &imap.FetchItemBodySection{}, `* 1 FETCH (BODY[] "" UID 42)`},
		{"header fields, no SP", headerRefs, "* 1 FETCH (BODY[HEADER.FIELDS (REFERENCES)] {0}\r\nUID 42)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()

			go func() {
				fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
				line, err := bufio.NewReader(serverConn).ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				fmt.Fprint(serverConn, tc.response+"\r\n"+tag+" OK FETCH completed\r\n")
			}()

			client := imapclient.New(clientConn, nil)
			defer client.Close()

			messages, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{
				UID:         true,
				BodySection: []*imap.FetchItemBodySection{tc.section},
			}).Collect()
			if err != nil {
				t.Fatalf("Fetch().Collect() = %v", err)
			}
			if len(messages) != 1 {
				t.Fatalf("len(messages) = %v, want 1", len(messages))
			}
			msg := messages[0]
			if msg.UID != 42 {
				t.Errorf("UID = %v, want 42", msg.UID)
			}
			if body := msg.FindBodySection(tc.section); body == nil || len(body) != 0 {
				t.Errorf("FindBodySection() = %q, want an empty, non-nil body", body)
			}
		})
	}

	// The same through Next, where the body is read as a stream.
	t.Run("streamed", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() {
			fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
			line, err := bufio.NewReader(serverConn).ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			fmt.Fprint(serverConn, "* 1 FETCH (BODY[] {0}\r\nUID 42)\r\n"+tag+" OK FETCH completed\r\n")
		}()
		client := imapclient.New(clientConn, nil)
		defer client.Close()

		cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{
			UID:         true,
			BodySection: []*imap.FetchItemBodySection{{}},
		})
		msg := cmd.Next()
		if msg == nil {
			t.Fatalf("Next() = nil, want the message: %v", cmd.Close())
		}
		var gotUID imap.UID
		var gotBody bool
		for item := msg.Next(); item != nil; item = msg.Next() {
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				gotUID = item.UID
			case imapclient.FetchItemDataBodySection:
				b, err := io.ReadAll(item.Literal)
				if err != nil || len(b) != 0 {
					t.Errorf("body = %q, %v; want empty", b, err)
				}
				gotBody = true
			}
		}
		if gotUID != 42 || !gotBody {
			t.Errorf("items: UID %v, body %v; want UID 42 and a body", gotUID, gotBody)
		}
		if err := cmd.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
}
