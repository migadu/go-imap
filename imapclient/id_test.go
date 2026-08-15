package imapclient

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadID covers both forms RFC 2971 §3.1 allows for the ID parameter: NIL,
// and a parenthesised key/value list.
//
// The list form regressed when the decoder stopped consuming input after a
// decode error: readID probed for NIL first, and on a list that probe left
// "expected atom, got \"(\"" recorded, so the list read that followed could no
// longer read a byte. Every ID exchange with a server that answers with its own
// name failed, and with it the connection's response stream.
func TestReadID(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string // expected IDData.Name
	}{
		{name: "list", data: " (\"name\" \"Sora\" \"version\" \"1.0\")\r\n", want: "Sora"},
		{name: "single pair", data: " (\"name\" \"Sora\")\r\n", want: "Sora"},
		{name: "nil", data: " NIL\r\n", want: ""},
		// RFC 2971 §3.1 types the value as nstring, so a server may answer NIL
		// for a field it declines to report. Reading values as strings rejected
		// the entire response over one such field.
		{name: "nil value", data: " (\"name\" NIL)\r\n", want: ""},
		{name: "nil value then real value", data: " (\"name\" NIL \"version\" \"1.0\")\r\n", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.data)), imapwire.ConnSideClient)

			data, err := (&Client{}).readID(dec)
			if err != nil {
				t.Fatalf("readID() = %v", err)
			}
			if data.Name != tc.want {
				t.Errorf("IDData.Name = %q, want %q", data.Name, tc.want)
			}
		})
	}
}
