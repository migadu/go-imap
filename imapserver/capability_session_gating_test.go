package imapserver

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// rev2CapSession implements SessionIMAP4rev2 (base Session + NAMESPACE + MOVE)
// and SessionUnauthenticate. The embedded Session is nil and must never be
// called; availableCaps only type-asserts the concrete type.
type rev2CapSession struct {
	Session
}

func (rev2CapSession) Namespace() (*imap.NamespaceData, error) { return nil, nil }
func (rev2CapSession) Move(w *MoveWriter, numSet imap.NumSet, dest string) error {
	return nil
}
func (rev2CapSession) Unauthenticate() error { return nil }

// baseCapSession implements only the base Session interface, modelling a
// backend that has not (yet) implemented IMAP4rev2 / NAMESPACE / MOVE /
// UNAUTHENTICATE.
type baseCapSession struct {
	Session
}

// TestAvailableCapsGatedOnSessionInterface is a regression test for the rule
// that IMAP4rev2, NAMESPACE, MOVE and UNAUTHENTICATE are advertised only when
// the session implements the corresponding interface. Previously these were
// advertised purely from configuration and a per-connection panic enforced the
// invariant; now they degrade gracefully like every other backend-dependent
// capability, so a misconfiguration can no longer take down connections.
func TestAvailableCapsGatedOnSessionInterface(t *testing.T) {
	caps := imap.CapSet{
		imap.CapIMAP4rev1:      {},
		imap.CapIMAP4rev2:      {},
		imap.CapNamespace:      {},
		imap.CapMove:           {},
		imap.CapUnauthenticate: {},
	}
	srv := &Server{options: Options{Caps: caps}}

	gated := []imap.Cap{
		imap.CapIMAP4rev2,
		imap.CapNamespace,
		imap.CapMove,
		imap.CapUnauthenticate,
	}

	// A session that implements the interfaces advertises all of them.
	full := &Conn{server: srv, state: imap.ConnStateAuthenticated, session: rev2CapSession{}}
	fullCaps := full.availableCaps()
	for _, want := range gated {
		if !hasCap(fullCaps, want) {
			t.Errorf("rev2-capable session: %q not advertised, got %v", want, fullCaps)
		}
	}

	// A session lacking the interfaces must NOT advertise them, must still
	// advertise IMAP4rev1, and must not panic.
	base := &Conn{server: srv, state: imap.ConnStateAuthenticated, session: baseCapSession{}}
	baseCaps := base.availableCaps() // must not panic
	if !hasCap(baseCaps, imap.CapIMAP4rev1) {
		t.Errorf("base session: IMAP4rev1 not advertised, got %v", baseCaps)
	}
	for _, notWant := range gated {
		if hasCap(baseCaps, notWant) {
			t.Errorf("base session: %q advertised despite missing session interface, got %v", notWant, baseCaps)
		}
	}
}
