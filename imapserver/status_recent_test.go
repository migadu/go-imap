package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// I4: STATUS in the wrong state must yield a state error, not the RECENT
// "Unknown STATUS data item" rejection — the state check now runs first.
func TestStatusRecentWrongStateReturnsStateError(t *testing.T) {
	c := &Conn{
		state:   imap.ConnStateNotAuthenticated,
		enabled: make(imap.CapSet),
		server:  New(&Options{Caps: imap.CapSet{imap.CapIMAP4rev2: {}}}),
	}
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(" INBOX (RECENT)\r\n")), imapwire.ConnSideServer)

	err := c.handleStatus(dec)
	if err == nil {
		t.Fatal("expected an error for STATUS in the not-authenticated state")
	}
	if strings.Contains(err.Error(), "Unknown STATUS data item") {
		t.Fatalf("RECENT rejection ran before the state check: %v", err)
	}
	if !strings.Contains(err.Error(), "only valid in") {
		t.Fatalf("expected a state error, got: %v", err)
	}
}
