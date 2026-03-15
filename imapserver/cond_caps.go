package imapserver

import (
	"github.com/emersion/go-imap/v2"
)

// supportsCondStore returns true if the connection supports CONDSTORE extension.
// This checks both session-specific capabilities (if available) and server capabilities,
// as well as enabled capabilities.
//
// Note: the previous iOS Mail workaround that disabled CONDSTORE for iOS clients
// has been removed.  That workaround caused a capability lie (CONDSTORE was
// advertised but then silently disabled) and the underlying server-side protocol
// bugs that originally caused iOS compatibility issues have since been fixed:
//   - MODSEQ in FETCH responses now correctly uses the "MODSEQ (value)" form
//     (RFC 7162 §2.3.2).
//   - ESEARCH MODSEQ result now uses the bare "MODSEQ value" form (RFC 7162
//     §3.4).
//   - UID FETCH CHANGEDSINCE+VANISHED now uses the canonical parenthesised
//     form (RFC 4466 §2.2 / RFC 7162 §6).
func (c *Conn) supportsCondStore() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		return sessionCaps.Has(imap.CapCondStore) || sessionCaps.Has(imap.CapIMAP4rev2)
	}
	return c.enabled.Has(imap.CapIMAP4rev2) || c.availableCapsSet().Has(imap.CapCondStore) || c.availableCapsSet().Has(imap.CapIMAP4rev2)
}

// supportsQResync returns true if the connection supports QRESYNC extension.
// This checks both session-specific capabilities (if available) and server capabilities.
func (c *Conn) supportsQResync() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		return sessionCaps.Has(imap.CapQResync)
	}
	return c.availableCapsSet().Has(imap.CapQResync)
}
