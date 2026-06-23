package imapserver

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestUseQuotedUTF8 is a regression test for the rule that UTF-8 mailbox names
// (RFC 9051 Net-Unicode) must only be used once the client has negotiated them
// via ENABLE IMAP4rev2 / ENABLE UTF8=ACCEPT, NOT merely because IMAP4rev2 is
// advertised. Gating on the advertised capability would send UTF-8 names to a
// legacy IMAP4rev1 client that never enabled IMAP4rev2 (RFC 9051 Section 5.1,
// RFC 5161). A server that does not advertise IMAP4rev1 has no legacy clients,
// so UTF-8 applies unconditionally.
func TestUseQuotedUTF8(t *testing.T) {
	rev1rev2 := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
	rev1only := imap.CapSet{imap.CapIMAP4rev1: {}}
	rev2only := imap.CapSet{imap.CapIMAP4rev2: {}}

	tests := []struct {
		name       string
		advertised imap.CapSet
		enabled    imap.CapSet
		want       bool
	}{
		// The regression: dual-stack server, client has not enabled rev2.
		{"dual-stack, nothing enabled (legacy client)", rev1rev2, imap.CapSet{}, false},
		{"dual-stack, IMAP4rev2 enabled", rev1rev2, imap.CapSet{imap.CapIMAP4rev2: {}}, true},
		{"dual-stack, UTF8=ACCEPT enabled", rev1rev2, imap.CapSet{imap.CapUTF8Accept: {}}, true},
		{"rev1-only, nothing enabled", rev1only, imap.CapSet{}, false},
		{"rev1-only, UTF8=ACCEPT enabled", rev1only, imap.CapSet{imap.CapUTF8Accept: {}}, true},
		// rev2-only server: no legacy clients to protect, UTF-8 from the start.
		{"rev2-only, nothing enabled", rev2only, imap.CapSet{}, true},
		{"rev2-only, IMAP4rev2 enabled", rev2only, imap.CapSet{imap.CapIMAP4rev2: {}}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{
				server:  &Server{options: Options{Caps: tc.advertised}},
				enabled: tc.enabled,
			}
			if got := c.useQuotedUTF8(); got != tc.want {
				t.Errorf("useQuotedUTF8() = %v, want %v (advertised=%v enabled=%v)",
					got, tc.want, tc.advertised, tc.enabled)
			}
		})
	}
}
