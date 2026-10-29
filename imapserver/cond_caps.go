package imapserver

import (
	"github.com/emersion/go-imap/v2"
)

// supportsCondStore returns true if the connection supports CONDSTORE extension.
// This checks session-specific capabilities (if available), or else server
// capabilities. QRESYNC implies CONDSTORE; IMAP4rev2 does not (RFC 9051
// Appendix F), so a server that offers CONDSTORE lists it, and what this
// accepts is what CAPABILITY advertises.
//
// Note: the previous iOS Mail workaround that disabled CONDSTORE for iOS clients
// has been removed.  That workaround caused a capability lie (CONDSTORE was
// advertised but then silently disabled) and the underlying server-side protocol
// bugs that originally caused iOS compatibility issues have since been fixed:
//   - MODSEQ in FETCH responses now correctly uses the "MODSEQ (value)" form
//     (RFC 7162 §3.1.4).
//   - ESEARCH MODSEQ result now uses the bare "MODSEQ value" form (RFC 4731
//     §3.2, RFC 7162 §3.1.10).
//   - UID FETCH CHANGEDSINCE+VANISHED now uses the canonical parenthesised
//     form (RFC 4466 §2.4 / RFC 7162 §3.2.6).
func (c *Conn) supportsCondStore() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		return capSession.GetCapabilities().Has(imap.CapCondStore)
	}
	// Read the server's configured capabilities directly rather than
	// availableCapsSet(), which is connection-state dependent and reads
	// c.state/c.conn without synchronization. supportsCondStore is reached
	// from the NOTIFY pump goroutine (via WriteMessageFlags and
	// UpdateWriter.CondStoreEnabled) concurrently with command-goroutine state
	// transitions; the configured CapSet is immutable after construction and
	// CONDSTORE is a backend-support capability that does not vary by state,
	// so this is both race-free and equivalent in the authenticated/selected
	// states where the pump runs.
	return c.server.options.caps().Has(imap.CapCondStore)
}

// markCondStoreEnabled records that the client has issued a CONDSTORE-enabling
// command (RFC 7162 §3.1) on this connection: SELECT/EXAMINE ... (CONDSTORE), a
// FETCH/SEARCH carrying a MODSEQ or CHANGEDSINCE modifier, or a STORE with
// UNCHANGEDSINCE. ENABLE CONDSTORE/QRESYNC is tracked separately in the enabled
// capability set (see handleEnable) and picked up by CondStoreEnabled.
func (c *Conn) markCondStoreEnabled() {
	c.mutex.Lock()
	c.condStore = true
	c.mutex.Unlock()
}

// CondStoreEnabled reports whether the client has become CONDSTORE-aware on this
// connection, either implicitly via a CONDSTORE-enabling command (see
// markCondStoreEnabled) or explicitly via ENABLE CONDSTORE/QRESYNC.
//
// Per RFC 7162 §3.2, MODSEQ data items in FETCH responses — both solicited
// STORE/FETCH replies and unsolicited flag updates — must only be sent to
// CONDSTORE-aware clients; sending them to a client that never enabled CONDSTORE
// breaks strict parsers such as mbsync/isync. Callers that emit MODSEQ should also
// confirm the session still advertises CONDSTORE (supportsCondStore), so a
// capability filter can suppress it mid-connection.
func (c *Conn) CondStoreEnabled() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.condStore || c.enabled.Has(imap.CapCondStore) || c.enabled.Has(imap.CapQResync)
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
