package imapserver

import (
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
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

// markCondStoreEnabled records that the client is parsing a CONDSTORE-enabling
// command (RFC 7162 §3.1) on this connection: SELECT/EXAMINE ... (CONDSTORE),
// STATUS (HIGHESTMODSEQ) (also as a LIST RETURN (STATUS) item), a FETCH/SEARCH
// carrying a MODSEQ or CHANGEDSINCE modifier, NOTIFY MessageNew (... MODSEQ ...),
// or a STORE with UNCHANGEDSINCE. ENABLE CONDSTORE/QRESYNC is tracked
// separately in the enabled capability set (see handleEnable) and picked up by
// CondStoreEnabled.
//
// The mark stays pending (invisible to the background NOTIFY pump) until the
// command succeeds (commitCondStore) and is discarded if the command fails
// (clearPendingCondStore).
func (c *Conn) markCondStoreEnabled() {
	c.mutex.Lock()
	c.condStorePending = true
	c.mutex.Unlock()
}

func (c *Conn) commitCondStore() {
	c.mutex.Lock()
	if c.condStorePending {
		c.condStore = true
		c.condStorePending = false
	}
	c.mutex.Unlock()
}

func (c *Conn) clearPendingCondStore() {
	c.mutex.Lock()
	c.condStorePending = false
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
	return c.condStore || c.condStorePending || c.enabled.Has(imap.CapCondStore) || c.enabled.Has(imap.CapQResync)
}

func (c *Conn) committedCondStoreEnabled() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.condStore || c.enabled.Has(imap.CapCondStore) || c.enabled.Has(imap.CapQResync)
}

// finishCondStore is called before the tagged response resp of a command,
// after the command's own responses and its post-command poll. wasCondStore
// and wasSelected describe the connection when the command started.
//
// A command answered BAD was refused as a protocol error (RFC 9051 §2.2.2),
// not executed, so it leaves the client as it was: the enabling commands mark
// CONDSTORE while they parse, before the rest of the command has been checked.
//
// Otherwise, with a mailbox selected that did not answer NOMODSEQ, it writes
// the mailbox's HIGHESTMODSEQ as the session reports it (SessionHighestModSeq)
// in two cases:
//
//   - The command made the client CONDSTORE-aware and began with the mailbox
//     selected. RFC 7162 §3.1: "A first CONDSTORE enabling command executed
//     in the session with a mailbox selected MUST cause the server to return
//     HIGHESTMODSEQ ... for the mailbox (if any is selected), unless the
//     server has sent a NOMODSEQ ... response code when the currently
//     selected mailbox was selected." Comparing CondStoreEnabled before and
//     after the command covers every enabling command, ENABLE included. A
//     successful SELECT/EXAMINE writes its own HIGHESTMODSEQ or NOMODSEQ and
//     its own tagged response, so it never gets here.
//
//   - A MODSEQ FETCH data item written since the last tagged response is
//     above it: the session still holds a change below that item, such as an
//     EXPUNGE that a FETCH, STORE or SEARCH may not send (RFC 9051 §7.5.1)
//     and whatever is queued behind it. The client takes "the highest value
//     among all MODSEQ FETCH data items received since the last tagged
//     response" unless it gets a HIGHESTMODSEQ response code, which it "MUST
//     use ... even if it has seen higher mod-sequences" (RFC 7162 §6). RFC
//     7162 §3.2: "if the server sends MODSEQ FETCH data items while EXPUNGE
//     (or VANISHED) replies with lower mod-sequences being delayed, the
//     server MUST send the HIGHESTMODSEQ response code with a lower value
//     than the EXPUNGE's mod-sequence."
//
// The response follows the command's own responses and its post-command poll
// (RFC 7162 does not place it within the command), so the session can report
// every change those delivered.
func (c *Conn) finishCondStore(enc *imapwire.Encoder, resp *imap.StatusResponse, wasCondStore, wasSelected bool) error {
	if resp.Type == imap.StatusResponseTypeOK {
		c.commitCondStore()
	} else {
		c.clearPendingCondStore()
	}
	firstEnabling := resp.Type == imap.StatusResponseTypeOK && !wasCondStore && wasSelected
	fetchModSeq := c.fetchModSeq.Load()
	if !firstEnabling && fetchModSeq == 0 {
		return nil
	}
	if c.state != imap.ConnStateSelected || c.selectedNoModSeq || !c.supportsCondStore() || !c.CondStoreEnabled() {
		return nil
	}
	session, ok := c.session.(SessionHighestModSeq)
	if !ok {
		return nil
	}
	modSeq := session.HighestModSeq()
	if modSeq == 0 || (!firstEnabling && fetchModSeq <= modSeq) {
		return nil
	}
	return writeHighestModSeq(enc, modSeq)
}

// flushClosingCondStore writes a compensating HIGHESTMODSEQ response code if
// MODSEQ FETCH data items written since the last tagged response exceed the
// currently selected mailbox's safe watermark (RFC 7162 §3.2, §3.2.8, §3.2.11,
// §6), and resets the span before the mailbox is closed.
//
// Callers must stop or fence the NOTIFY pump before calling this so no further
// unsolicited FETCH responses for the closing mailbox can be written afterward.
func (c *Conn) flushClosingCondStore() error {
	fetchModSeq := c.fetchModSeq.Swap(0)
	if fetchModSeq == 0 {
		return nil
	}
	if c.state != imap.ConnStateSelected || c.selectedNoModSeq || !c.supportsCondStore() || !c.CondStoreEnabled() {
		return nil
	}
	session, ok := c.session.(SessionHighestModSeq)
	if !ok {
		return nil
	}
	modSeq := session.HighestModSeq()
	if modSeq == 0 || fetchModSeq <= modSeq {
		return nil
	}
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeHighestModSeq(enc.Encoder, modSeq)
}

// noteFetchModSeq records that a MODSEQ FETCH data item was written, for
// finishCondStore.
func (c *Conn) noteFetchModSeq(modSeq uint64) {
	for {
		seen := c.fetchModSeq.Load()
		if modSeq <= seen || c.fetchModSeq.CompareAndSwap(seen, modSeq) {
			return
		}
	}
}

// supportsESearch reports whether the session supports ESEARCH (RFC 4731),
// which IMAP4rev2 includes.
func (c *Conn) supportsESearch() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		return sessionCaps.Has(imap.CapESearch) || sessionCaps.Has(imap.CapIMAP4rev2)
	}
	return c.availableCapsSet().Has(imap.CapESearch) || c.availableCapsSet().Has(imap.CapIMAP4rev2)
}

// supportsSearchRes reports whether SAVE is supported (RFC 5182). Has includes
// SEARCHRES when the session supports IMAP4rev2 (RFC 9051 Appendix E).
func (c *Conn) supportsSearchRes() bool {
	return c.configuredCaps().Has(imap.CapSearchRes)
}

// supportsESort reports whether the session supports ESORT (RFC 5267), which
// IMAP4rev2 does not include.
func (c *Conn) supportsESort() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		return capSession.GetCapabilities().Has(imap.CapESort)
	}
	return c.availableCapsSet().Has(imap.CapESort)
}

// supportsWithin reports whether the session supports the OLDER and YOUNGER
// search keys (RFC 5032).
func (c *Conn) supportsWithin() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		return capSession.GetCapabilities().Has(imap.CapWithin)
	}
	return c.availableCapsSet().Has(imap.CapWithin)
}

// supportsPreview reports whether the session supports the PREVIEW FETCH data
// item (RFC 8970).
func (c *Conn) supportsPreview() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		return capSession.GetCapabilities().Has(imap.CapPreview)
	}
	return c.availableCapsSet().Has(imap.CapPreview)
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
