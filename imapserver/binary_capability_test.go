package imapserver

import (
	"testing"

	"github.com/migadu/go-imap"
)

// rev2SessionCaps is a rev2-capable session that reports its own capability
// set (SessionCapabilities), which replaces the server-wide Options.Caps.
type rev2SessionCaps struct {
	rev2CapSession
	caps imap.CapSet
}

func (s rev2SessionCaps) GetCapabilities() imap.CapSet { return s.caps }

// RFC 9051 Appendix B: IMAP4rev2 includes BINARY's FETCH items "but not
// extensions to the APPEND command. IMAP4rev2 implementations that support full
// [RFC3516] functionality need to also advertise the BINARY capability". BINARY
// is therefore advertised wherever it is configured, whichever base protocols
// are offered, and never implied by IMAP4rev2 alone.
func TestBinaryCapabilityAdvertisedWhenConfigured(t *testing.T) {
	rev1 := imap.CapSet{imap.CapIMAP4rev1: {}}
	rev2 := imap.CapSet{imap.CapIMAP4rev2: {}}
	mixed := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
	withBinary := func(base imap.CapSet) imap.CapSet {
		caps := base.Copy()
		caps[imap.CapBinary] = struct{}{}
		return caps
	}

	for _, tc := range []struct {
		name string
		caps imap.CapSet
		want bool
	}{
		{"rev1 only", rev1, false},
		{"rev1 only with BINARY", withBinary(rev1), true},
		{"rev2 only", rev2, false},
		{"rev2 only with BINARY", withBinary(rev2), true},
		{"rev1 and rev2", mixed, false},
		{"rev1 and rev2 with BINARY", withBinary(mixed), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, src := range []struct {
				name string
				conn func(state imap.ConnState) *Conn
			}{
				{"Options.Caps", func(state imap.ConnState) *Conn {
					srv := &Server{options: Options{Caps: tc.caps}}
					return &Conn{server: srv, state: state, session: rev2CapSession{}}
				}},
				{"SessionCapabilities", func(state imap.ConnState) *Conn {
					srv := &Server{options: Options{Caps: mixed}}
					return &Conn{server: srv, state: state, session: rev2SessionCaps{caps: tc.caps}}
				}},
			} {
				t.Run(src.name, func(t *testing.T) {
					for _, state := range []imap.ConnState{imap.ConnStateAuthenticated, imap.ConnStateSelected} {
						caps := src.conn(state).availableCaps()
						if got := hasCap(caps, imap.CapBinary); got != tc.want {
							t.Errorf("state %v: BINARY advertised = %v, want %v; got %v", state, got, tc.want, caps)
						}
					}
					// As before, BINARY is an authenticated-state capability.
					caps := src.conn(imap.ConnStateNotAuthenticated).availableCaps()
					if hasCap(caps, imap.CapBinary) {
						t.Errorf("not authenticated: BINARY advertised, got %v", caps)
					}
				})
			}
		})
	}
}
