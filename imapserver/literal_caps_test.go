package imapserver

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestLiteralCapsAreMutuallyExclusive pins RFC 7888 §3: a server MUST NOT
// advertise LITERAL- and LITERAL+ at the same time. Before the fix a server
// configured with LITERAL+ appended LITERAL- unconditionally under IMAP4rev1 and
// then LITERAL+ once authenticated, so every post-LOGIN capability list carried
// both.
//
// The contract this pins: LITERAL- (and only LITERAL-) before authentication,
// whatever is configured; LITERAL+ (and only LITERAL+) once authenticated when it
// is configured; LITERAL- stays when it is not. Never both.
func TestLiteralCapsAreMutuallyExclusive(t *testing.T) {
	preAuth := []imap.ConnState{imap.ConnStateNotAuthenticated}
	postAuth := []imap.ConnState{imap.ConnStateAuthenticated, imap.ConnStateSelected}

	cases := []struct {
		name         string
		caps         imap.CapSet
		session      Session
		wantPostPlus bool // LITERAL+ expected once authenticated
	}{
		{
			name:         "rev1 with LITERAL+",
			caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapLiteralPlus: {}},
			session:      baseCapSession{},
			wantPostPlus: true,
		},
		{
			name:         "rev1 without LITERAL+",
			caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
			session:      baseCapSession{},
			wantPostPlus: false,
		},
		{
			name: "dual stack with LITERAL+",
			caps: imap.CapSet{
				imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {},
				imap.CapNamespace: {}, imap.CapMove: {}, imap.CapLiteralPlus: {},
			},
			session:      rev2CapSession{},
			wantPostPlus: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{options: Options{Caps: tc.caps, InsecureAuth: true}}

			for _, state := range preAuth {
				caps := (&Conn{server: srv, state: state, session: tc.session}).availableCaps()
				if !hasCap(caps, imap.CapLiteralMinus) {
					t.Errorf("state %v: LITERAL- not advertised before authentication: %v", state, caps)
				}
				if hasCap(caps, imap.CapLiteralPlus) {
					t.Errorf("state %v: LITERAL+ advertised before authentication: %v", state, caps)
				}
			}
			for _, state := range postAuth {
				caps := (&Conn{server: srv, state: state, session: tc.session}).availableCaps()
				plus, minus := hasCap(caps, imap.CapLiteralPlus), hasCap(caps, imap.CapLiteralMinus)
				if plus && minus {
					t.Errorf("state %v: LITERAL+ and LITERAL- advertised together (RFC 7888 §3): %v", state, caps)
				}
				if plus != tc.wantPostPlus {
					t.Errorf("state %v: LITERAL+ advertised = %v, want %v: %v", state, plus, tc.wantPostPlus, caps)
				}
				if minus == tc.wantPostPlus {
					t.Errorf("state %v: LITERAL- advertised = %v, want %v: %v", state, minus, !tc.wantPostPlus, caps)
				}
			}
		})
	}
}

// TestNonSyncLiteralCapFollowsAdvertisedLiteralPlus pins that acceptLiteral's
// 4096-byte bound on non-synchronizing literals tracks the ADVERTISED
// capability, not the configured one: a server configured with LITERAL+ still
// advertises LITERAL- before authentication, so the bound applies there and is
// lifted only once the client is authenticated.
func TestNonSyncLiteralCapFollowsAdvertisedLiteralPlus(t *testing.T) {
	srv := &Server{options: Options{
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapLiteralPlus: {}},
		InsecureAuth: true,
	}}

	pre := &Conn{server: srv, state: imap.ConnStateNotAuthenticated, session: baseCapSession{}}
	if err := pre.acceptLiteral(4097, true); err == nil {
		t.Error("unauthenticated: 4097-byte non-sync literal accepted while LITERAL- is advertised")
	} else if e, ok := err.(*imap.Error); !ok || e.Type != imap.StatusResponseTypeBad {
		t.Errorf("unauthenticated: got %v, want a BAD", err)
	}
	if err := pre.acceptLiteral(4096, true); err != nil {
		t.Errorf("unauthenticated: 4096-byte non-sync literal refused: %v", err)
	}

	post := &Conn{server: srv, state: imap.ConnStateAuthenticated, session: baseCapSession{}}
	if err := post.acceptLiteral(4097, true); err != nil {
		t.Errorf("authenticated with LITERAL+: 4097-byte non-sync literal refused: %v", err)
	}

	// Without LITERAL+ configured the bound holds in every state, as before.
	noPlus := &Server{options: Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}, InsecureAuth: true}}
	c := &Conn{server: noPlus, state: imap.ConnStateAuthenticated, session: baseCapSession{}}
	if err := c.acceptLiteral(4097, true); err == nil {
		t.Error("authenticated without LITERAL+: 4097-byte non-sync literal accepted")
	}
}
