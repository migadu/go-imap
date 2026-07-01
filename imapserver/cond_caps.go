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
