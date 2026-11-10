package imapserver

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// TestAvailableCapsAdvertiseEveryThreadAlgorithm checks that each algorithm
// the THREAD command accepts is advertised when the server is configured with
// it. A client picks its algorithm from the THREAD= capabilities, so one that
// is accepted but never advertised is never used.
func TestAvailableCapsAdvertiseEveryThreadAlgorithm(t *testing.T) {
	algorithms := []imap.ThreadAlgorithm{
		imap.ThreadOrderedSubject,
		imap.ThreadReferences,
		imap.ThreadRefs,
	}

	caps := imap.CapSet{imap.CapIMAP4rev1: {}}
	for _, alg := range algorithms {
		caps[imap.Cap("THREAD="+string(alg))] = struct{}{}
	}
	conn := &Conn{
		server:  &Server{options: Options{Caps: caps}},
		state:   imap.ConnStateAuthenticated,
		session: baseCapSession{},
	}

	advertised := make(imap.CapSet)
	for _, c := range conn.availableCaps() {
		advertised[c] = struct{}{}
	}
	for _, alg := range algorithms {
		if want := imap.Cap("THREAD=" + string(alg)); !advertised.Has(want) {
			t.Errorf("%q configured but not advertised", want)
		}
	}
	if got := advertised.ThreadAlgorithms(); len(got) != len(algorithms) {
		t.Errorf("ThreadAlgorithms() = %v, want %v", got, algorithms)
	}

	// Not configured, not advertised.
	bare := &Conn{
		server:  &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}},
		state:   imap.ConnStateAuthenticated,
		session: baseCapSession{},
	}
	for _, c := range bare.availableCaps() {
		if strings.HasPrefix(string(c), "THREAD=") {
			t.Errorf("%q advertised without being configured", c)
		}
	}
}
