package imapclient_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// A number too large for its type, where the grammar has a number or
// something else, is a malformed response, not the absence of a number. The
// decoder read the digits and reported no number, so parsing went on with the
// alternative: the THREAD lost a message, APPENDLIMIT took the NIL glued to the
// number, and BODY[4294967296] became the whole message. Issue #168.
func TestResponseNumberOverflow(t *testing.T) {
	// RFC 5256 section 5: thread-members contains nz-number values, both
	// in the top-level chain and inside nested thread-lists.
	for _, members := range []string{"4294967296(2)", "0", "1 0", "(0)(2)"} {
		t.Run("THREAD "+members, func(t *testing.T) {
			client := newCodeServer(t, "* THREAD ("+members+")\r\n")
			client.SetStateForTest(imap.ConnStateSelected)
			data, err := client.Thread(&imapclient.ThreadOptions{
				Algorithm:      imap.ThreadOrderedSubject,
				SearchCriteria: &imap.SearchCriteria{},
			}).Wait()
			if err == nil {
				t.Errorf("Thread() = %+v, nil; want an error for %s", data, members)
			}
		})
	}

	for _, value := range []string{"4294967296", "9223372036854775808NIL"} {
		t.Run("STATUS APPENDLIMIT "+value, func(t *testing.T) {
			client := newCodeServer(t, "* STATUS INBOX (APPENDLIMIT "+value+")\r\n")
			client.SetStateForTest(imap.ConnStateAuthenticated)
			data, err := client.Status("INBOX", &imap.StatusOptions{AppendLimit: true}).Wait()
			if err == nil {
				t.Fatalf("Status() = %+v (APPENDLIMIT %v), nil; want an error", data, *data.AppendLimit)
			}
			if !strings.Contains(err.Error(), "number too large") {
				t.Errorf("Status() = %v, want it to name the oversized number", err)
			}
		})
	}

	for _, item := range []string{"BODY[4294967296]", "BODY[4294967296HEADER]", "BODY[0]", "BODY[1.0]", "BODY[0.HEADER]", "BINARY[0]", "BINARY[1.0]", "BINARY[4294967296]"} {
		t.Run("FETCH "+item, func(t *testing.T) {
			client := newCodeServer(t, "* 1 FETCH ("+item+" {5}\r\nhello)\r\n")
			client.SetStateForTest(imap.ConnStateSelected)
			msgs, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
				BodySection: []*imap.FetchItemBodySection{{}},
			}).Collect()
			if err == nil {
				t.Errorf("Fetch() = %d messages, nil; want an error for %s", len(msgs), item)
			}
		})
	}
	for _, part := range []string{"0", "1.0", "4294967296"} {
		t.Run("FETCH BINARY.SIZE["+part+"]", func(t *testing.T) {
			client := newCodeServer(t, "* 1 FETCH (BINARY.SIZE["+part+"] 5)\r\n")
			client.SetStateForTest(imap.ConnStateSelected)
			if _, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{BinarySectionSize: []*imap.FetchItemBinarySectionSize{{}}}).Collect(); err == nil {
				t.Errorf("BINARY.SIZE[%s] was accepted", part)
			}
		})
	}

	// A number that fits is still read, NIL still means no limit.
	for value, want := range map[string]uint32{"0": 0, "4294967295": 4294967295, "NIL": 4294967295} {
		t.Run("STATUS in range "+value, func(t *testing.T) {
			client := newCodeServer(t, "* STATUS INBOX (APPENDLIMIT "+value+")\r\n")
			client.SetStateForTest(imap.ConnStateAuthenticated)
			data, err := client.Status("INBOX", &imap.StatusOptions{AppendLimit: true}).Wait()
			if err != nil || data.AppendLimit == nil || *data.AppendLimit != want {
				t.Errorf("Status() = %+v, %v; want APPENDLIMIT %v", data, err, want)
			}
		})
	}
	t.Run("THREAD in range", func(t *testing.T) {
		client := newCodeServer(t, "* THREAD (4294967295 2)\r\n")
		client.SetStateForTest(imap.ConnStateSelected)
		data, err := client.Thread(&imapclient.ThreadOptions{Algorithm: imap.ThreadOrderedSubject, SearchCriteria: &imap.SearchCriteria{}}).Wait()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 1 || len(data[0].Chain) != 2 || data[0].Chain[0] != 4294967295 || data[0].Chain[1] != 2 {
			t.Errorf("Thread() = %+v, want the maximum number and 2 in one chain", data)
		}
	})
	t.Run("FETCH maximum section", func(t *testing.T) {
		if strconv.IntSize < 64 {
			t.Skip("section parts use int, which cannot hold every uint32 on a 32-bit target")
		}
		maxPart := uint32(4294967295)
		part := int(maxPart)
		client := newCodeServer(t, "* 1 FETCH (BODY[4294967295] {5}\r\nhello)\r\n")
		client.SetStateForTest(imap.ConnStateSelected)
		data, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Part: []int{part}}}}).Collect()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 1 || len(data[0].BodySection) != 1 {
			t.Fatalf("Fetch() = %+v, want one body section", data)
		}
		body := data[0].BodySection[0]
		if len(body.Section.Part) != 1 || body.Section.Part[0] != part || string(body.Bytes) != "hello" {
			t.Errorf("Fetch section = %+v, data = %q", body.Section, body.Bytes)
		}
	})
}
