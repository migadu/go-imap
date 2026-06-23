package imapserver

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// additionalCapsSession is a minimal session implementing SessionAdditionalCaps.
// The embedded Session is nil and must never be called; availableCaps() only
// needs the type to satisfy the interface via a type assertion.
type additionalCapsSession struct {
	Session
	extra []imap.Cap
}

func (s additionalCapsSession) AdditionalCapabilities() []imap.Cap { return s.extra }

// TestAdditionalCapsAdvertisedInAllStates verifies a session's extra capability
// tokens are advertised verbatim, including in the unauthenticated state (the
// greeting), where the standard backend-gated capabilities are not emitted.
func TestAdditionalCapsAdvertisedInAllStates(t *testing.T) {
	srv := &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}}
	custom := imap.Cap("X-ICEWARP-SERVER")

	for _, state := range []imap.ConnState{
		imap.ConnStateNotAuthenticated,
		imap.ConnStateAuthenticated,
		imap.ConnStateSelected,
	} {
		conn := &Conn{server: srv, state: state, session: additionalCapsSession{extra: []imap.Cap{custom}}}
		caps := conn.availableCaps()
		if !hasCap(caps, custom) {
			t.Errorf("state %v: custom capability %q not advertised, got %v", state, custom, caps)
		}
	}
}

// TestAdditionalCapsDeduped verifies tokens already present in the standard set
// are not duplicated, and empty tokens are skipped.
func TestAdditionalCapsDeduped(t *testing.T) {
	srv := &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}}
	conn := &Conn{
		server:  srv,
		state:   imap.ConnStateNotAuthenticated,
		session: additionalCapsSession{extra: []imap.Cap{imap.CapIMAP4rev1, "", "X-CUSTOM"}},
	}

	caps := conn.availableCaps()

	count := 0
	for _, c := range caps {
		if c == imap.CapIMAP4rev1 {
			count++
		}
		if c == "" {
			t.Errorf("empty capability token was advertised: %v", caps)
		}
	}
	if count != 1 {
		t.Errorf("IMAP4rev1 advertised %d times, want 1 (dedupe failed): %v", count, caps)
	}
	if !hasCap(caps, imap.Cap("X-CUSTOM")) {
		t.Errorf("X-CUSTOM not advertised, got %v", caps)
	}
}

// TestNonAdditionalCapsSessionUnaffected verifies a session not implementing
// SessionAdditionalCaps advertises no extra tokens.
func TestNonAdditionalCapsSessionUnaffected(t *testing.T) {
	srv := &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}}
	conn := &Conn{server: srv, state: imap.ConnStateNotAuthenticated, session: struct{ Session }{}}

	caps := conn.availableCaps()
	if hasCap(caps, imap.Cap("X-ICEWARP-SERVER")) {
		t.Errorf("unexpected custom capability for plain session: %v", caps)
	}
}
