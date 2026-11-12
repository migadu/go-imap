package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// The decoder tolerates the extra SPs some servers put inside parenthesized
// lists, as in "FLAGS (\Answered \Seen )". That leniency is for a client
// decoding server output. The server keeps the grammar (RFC 9051 has no SP
// after "(" or before ")"), so a client that sends such a list still gets BAD,
// and the backend never sees the command.
func TestServerListExtraSPIsBAD(t *testing.T) {
	c := newStoreTestConn(t, imap.CapSet{imap.CapIMAP4rev1: {}}, 1)

	for _, cmd := range []string{
		`STORE 1 +FLAGS (\Seen )`,
		`STORE 1 +FLAGS ( \Seen)`,
		`STORE 1 +FLAGS (\Seen  \Flagged)`,
	} {
		if resp := c.do("store", cmd); !strings.Contains(resp, "store BAD") {
			t.Errorf("%v = %q, want a tagged BAD", cmd, resp)
		}
	}

	if resp := c.do("store", `STORE 1 +FLAGS (\Seen \Flagged)`); !strings.Contains(resp, "store OK") {
		t.Errorf("well-formed STORE = %q, want OK", resp)
	}
}
