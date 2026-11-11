package imapclient_test

import (
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
	t.Run("THREAD", func(t *testing.T) {
		client := newCodeServer(t, "* THREAD (4294967296(2))\r\n")
		client.SetStateForTest(imap.ConnStateSelected)
		data, err := client.Thread(&imapclient.ThreadOptions{
			Algorithm:      imap.ThreadOrderedSubject,
			SearchCriteria: &imap.SearchCriteria{},
		}).Wait()
		if err == nil {
			t.Errorf("Thread() = %+v, nil; want an error for message number 4294967296", data)
		}
	})

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

	for _, item := range []string{"BODY[4294967296]", "BODY[4294967296HEADER]", "BODY[0]", "BINARY[0]"} {
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

	// A number that fits is still read, NIL still means no limit.
	t.Run("in range", func(t *testing.T) {
		client := newCodeServer(t, "* STATUS INBOX (APPENDLIMIT 4294967295)\r\n* THREAD (4294967295(2))\r\n")
		client.SetStateForTest(imap.ConnStateSelected)
		data, err := client.Status("INBOX", &imap.StatusOptions{AppendLimit: true}).Wait()
		if err != nil || data.AppendLimit == nil || *data.AppendLimit != 4294967295 {
			t.Errorf("Status() = %+v, %v; want APPENDLIMIT 4294967295", data, err)
		}
	})
}
