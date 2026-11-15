package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// TestStatusHighestModSeqEnablesCondStore verifies that requesting the
// HIGHESTMODSEQ status data item makes the client CONDSTORE-aware. RFC 7162
// §3.1 lists "a STATUS (HIGHESTMODSEQ) command" among the CONDSTORE enabling
// commands, after which "the server MUST then include mod-sequence data in all
// subsequent untagged FETCH responses (until the connection is closed)".
//
// LIST ... RETURN (STATUS (HIGHESTMODSEQ)) (RFC 5819) counts as well: it asks
// for the same STATUS data item and gets the same untagged STATUS response.
func TestStatusHighestModSeqEnablesCondStore(t *testing.T) {
	caps := imap.CapSet{
		imap.CapIMAP4rev1:  {},
		imap.CapCondStore:  {},
		imap.CapListStatus: {},
	}

	for _, tt := range []struct {
		name     string
		cmd      string
		wantResp string
		wantCS   bool
	}{
		{
			name:     "STATUS (HIGHESTMODSEQ)",
			cmd:      "STATUS INBOX (HIGHESTMODSEQ)",
			wantResp: "* STATUS INBOX (HIGHESTMODSEQ ",
			wantCS:   true,
		},
		{
			name:     "STATUS (MESSAGES HIGHESTMODSEQ)",
			cmd:      "STATUS INBOX (MESSAGES HIGHESTMODSEQ)",
			wantResp: "* STATUS INBOX (MESSAGES 1 HIGHESTMODSEQ ",
			wantCS:   true,
		},
		{
			name:     "LIST RETURN (STATUS (HIGHESTMODSEQ))",
			cmd:      `LIST "" INBOX RETURN (STATUS (HIGHESTMODSEQ))`,
			wantResp: "* STATUS INBOX (HIGHESTMODSEQ ",
			wantCS:   true,
		},
		{
			// Without HIGHESTMODSEQ, STATUS is not an enabling command.
			name:     "STATUS (MESSAGES)",
			cmd:      "STATUS INBOX (MESSAGES)",
			wantResp: "* STATUS INBOX (MESSAGES 1)",
			wantCS:   false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := newStoreTestConn(t, caps, 1)

			resp := c.do("status", tt.cmd)
			if !strings.Contains(resp, tt.wantResp) || !strings.Contains(resp, "status OK") {
				t.Fatalf("%v = %q, want %q and a tagged OK", tt.cmd, resp, tt.wantResp)
			}
			if got := c.srvConn.CondStoreEnabled(); got != tt.wantCS {
				t.Errorf("CondStoreEnabled() after %v = %v, want %v", tt.cmd, got, tt.wantCS)
			}

			resp = c.do("store", `STORE 1 +FLAGS (\Seen)`)
			if !containsFlag(resp, imap.FlagSeen) {
				t.Fatalf("STORE = %q, want the untagged FETCH with the new flags", resp)
			}
			if got := strings.Contains(resp, "MODSEQ ("); got != tt.wantCS {
				t.Errorf("STORE after %v = %q, MODSEQ present = %v, want %v", tt.cmd, resp, got, tt.wantCS)
			}
		})
	}
}
