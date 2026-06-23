package imapserver

import (
	"fmt"
	"slices"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleCapability(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("CAPABILITY")
	for _, c := range c.availableCaps() {
		enc.SP().Atom(string(c))
	}
	return enc.CRLF()
}

// availableCaps returns the capabilities supported by the server.
//
// They depend on the connection state.
//
// Some extensions (e.g. SASL-IR, ENABLE) don't require backend support and
// thus are always enabled.
func (c *Conn) availableCaps() []imap.Cap {
	available := c.server.options.caps()

	// If the session provides its own capabilities, it completely overrides
	// the server-wide ones.
	if capSession, ok := c.session.(SessionCapabilities); ok {
		available = capSession.GetCapabilities()
	}

	var caps []imap.Cap
	// IMAP4rev2 implies NAMESPACE and MOVE, so it requires the session to
	// implement SessionIMAP4rev2. Advertise it only when it is both configured
	// and supported: a dual-stack server whose session lacks rev2 support
	// degrades to IMAP4rev1 rather than advertising a version it cannot honor.
	// This matches how every other backend-dependent capability is gated below
	// (METADATA, ACL, ...) and avoids failing connections over a recoverable
	// configuration mistake. Operators that require rev2 should assert it at
	// compile time: var _ imapserver.SessionIMAP4rev2 = (*mySession)(nil).
	if _, ok := c.session.(SessionIMAP4rev2); ok && available.Has(imap.CapIMAP4rev2) {
		caps = append(caps, imap.CapIMAP4rev2)
	}
	if available.Has(imap.CapIMAP4rev1) {
		caps = append(caps, imap.CapIMAP4rev1)
	}
	if len(caps) == 0 {
		// Only reachable when the server advertises IMAP4rev2 alone (no
		// IMAP4rev1) but the session does not implement SessionIMAP4rev2,
		// leaving no usable base protocol. New() guarantees at least one of the
		// two versions is configured.
		panic("imapserver: server advertises IMAP4rev2 only but the session does not implement SessionIMAP4rev2")
	}

	if available.Has(imap.CapIMAP4rev1) {
		caps = append(caps, []imap.Cap{
			imap.CapSASLIR,
			imap.CapLiteralMinus,
		}...)
	}
	if c.canStartTLS() {
		caps = append(caps, imap.CapStartTLS)
	}
	if c.canAuth() {
		mechs := []string{"PLAIN"}
		if authSess, ok := c.session.(SessionSASL); ok {
			mechs = authSess.AuthenticateMechanisms()
		}
		for _, mech := range mechs {
			caps = append(caps, imap.Cap("AUTH="+mech))
		}
	} else if c.state == imap.ConnStateNotAuthenticated {
		caps = append(caps, imap.CapLoginDisabled)
	}
	if c.state == imap.ConnStateAuthenticated || c.state == imap.ConnStateSelected {
		if available.Has(imap.CapIMAP4rev1) {
			// IMAP4rev1-specific capabilities that don't require backend
			// support and are not applicable to IMAP4rev2
			caps = append(caps, []imap.Cap{
				imap.CapUnselect,
				imap.CapEnable,
				imap.CapUTF8Accept,
			}...)

			// IMAP4rev1-specific capabilities which require backend support
			// and are not applicable to IMAP4rev2
			addAvailableCaps(&caps, available, []imap.Cap{
				imap.CapIdle,
				imap.CapUIDPlus,
				imap.CapESearch,
				imap.CapSearchRes,
				imap.CapListExtended,
				imap.CapListStatus,
				imap.CapStatusSize,
				imap.CapBinary,
				imap.CapChildren,
			})

			// NAMESPACE and MOVE require optional session interfaces; advertise
			// them only when the session implements them (see IMAP4rev2 above).
			if _, ok := c.session.(SessionNamespace); ok && available.Has(imap.CapNamespace) {
				caps = append(caps, imap.CapNamespace)
			}
			if _, ok := c.session.(SessionMove); ok && available.Has(imap.CapMove) {
				caps = append(caps, imap.CapMove)
			}
		}

		// Capabilities which require backend support and apply to both
		// IMAP4rev1 and IMAP4rev2
		addAvailableCaps(&caps, available, []imap.Cap{
			imap.CapSpecialUse,
			imap.CapCreateSpecialUse,
			imap.CapLiteralPlus,
			imap.CapCondStore,
			imap.CapQResync,
			imap.CapSort,
			imap.CapSortDisplay,
			imap.CapESort,
			imap.CapID,
			imap.Cap("THREAD=REFERENCES"),
			imap.Cap("THREAD=ORDEREDSUBJECT"),
		})

		// UNAUTHENTICATE requires an optional session interface; advertise it
		// only when the session implements it.
		if _, ok := c.session.(SessionUnauthenticate); ok && available.Has(imap.CapUnauthenticate) {
			caps = append(caps, imap.CapUnauthenticate)
		}

		// METADATA capability
		if _, ok := c.session.(SessionMetadata); ok && available.Has(imap.CapMetadata) {
			caps = append(caps, imap.CapMetadata)
		}

		// MULTISEARCH capability
		if _, ok := c.session.(SessionMultiSearch); ok && available.Has(imap.Cap("MULTISEARCH")) {
			caps = append(caps, imap.Cap("MULTISEARCH"))
		}

		// Add ACL capability if the session supports it (RFC 4314). The
		// extension also requires advertising the "RIGHTS=" capability listing
		// the rights introduced by RFC 4314 (k, x, t, e); see Section 2.1.
		if _, ok := c.session.(SessionACL); ok {
			caps = append(caps, imap.CapACL)
			caps = append(caps, imap.Cap("RIGHTS="+imap.RightSetExtended.String()))
		}

		if appendLimitSession, ok := c.session.(SessionAppendLimit); ok {
			limit := appendLimitSession.AppendLimit()
			caps = append(caps, imap.Cap(fmt.Sprintf("APPENDLIMIT=%d", limit)))
		} else {
			addAvailableCaps(&caps, available, []imap.Cap{imap.CapAppendLimit})
		}
	}

	// Extra, non-standard tokens advertised verbatim by the session. Emitted in
	// every connection state (including the unauthenticated greeting) and not
	// gated by the known-capability allowlist above, so vendor/experimental
	// tokens reach the client. De-duplicated against the standard set.
	if extraSession, ok := c.session.(SessionAdditionalCaps); ok {
		for _, extra := range extraSession.AdditionalCapabilities() {
			if extra != "" && !slices.Contains(caps, extra) {
				caps = append(caps, extra)
			}
		}
	}

	return caps
}

func addAvailableCaps(caps *[]imap.Cap, available imap.CapSet, l []imap.Cap) {
	for _, c := range l {
		if available.Has(c) {
			*caps = append(*caps, c)
		}
	}
}

func (c *Conn) availableCapsSet() imap.CapSet {
	caps := c.availableCaps()
	capSet := make(imap.CapSet)
	for _, cap := range caps {
		capSet[cap] = struct{}{}
	}
	return capSet
}
