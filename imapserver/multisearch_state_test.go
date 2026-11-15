package imapserver_test

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/migadu/go-imap/imapserver"
)

// RFC 7377 section 2.2 requires selected state whenever the source includes
// (or defaults to) selected, even when other source options are also present.
func TestMultiSearchSourceState(t *testing.T) {
	for _, tc := range []struct {
		name, source, authenticatedStatus string
	}{
		{"default", "", "BAD"},
		{"selected", "IN (selected) ", "BAD"},
		{"selected personal", "IN (selected personal) ", "BAD"},
		{"selected inboxes", "IN (selected inboxes) ", "BAD"},
		{"personal", "IN (personal) ", "OK"},
		{"inboxes", "IN (inboxes) ", "OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, selected := range []bool{false, true} {
				state := "authenticated"
				wantStatus := tc.authenticatedStatus
				if selected {
					state = "selected"
					wantStatus = "OK"
				}
				t.Run(state, func(t *testing.T) {
					var calls atomic.Int32
					caps := multiSaveCaps()
					rc := newSearchRev2ConnWrapped(t, caps, func(s imapserver.Session) imapserver.Session {
						return &multiSaveSession{Session: s, caps: caps, calls: &calls}
					})
					rc.appendOne("one")
					if selected {
						rc.do("SELECT INBOX")
					}

					resp, tagged := rc.try("ESEARCH " + tc.source + "ALL")
					wantCalls := int32(1)
					if wantStatus == "BAD" {
						wantCalls = 0
						if !strings.Contains(tagged, " BAD [CLIENTBUG] This command is only valid in the selected state\r\n") || resp != tagged {
							t.Errorf("ESEARCH response = %q, want only tagged BAD for selected state", resp)
						}
					} else if !strings.Contains(tagged, " OK ") || !strings.Contains(resp, " ALL 1\r\n") {
						t.Errorf("ESEARCH response = %q, want OK with matching UID 1", resp)
					}
					if got := calls.Load(); got != wantCalls {
						t.Errorf("MultiSearch calls = %d, want %d", got, wantCalls)
					}
					rc.do("NOOP")
				})
			}
		})
	}
}
