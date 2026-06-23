package imapserver

import (
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleEnable(dec *imapwire.Decoder) error {
	var requested []imap.Cap
	for dec.SP() {
		cap, err := internal.ExpectCap(dec)
		if err != nil {
			return err
		}
		requested = append(requested, cap)
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	var enabled []imap.Cap
	for _, req := range requested {
		switch req {
		case imap.CapIMAP4rev2, imap.CapUTF8Accept:
			// Only enable if advertised, so the enabled set never outruns the
			// advertised capabilities (e.g. a client must not be able to enable
			// IMAP4rev2 on a session that does not implement SessionIMAP4rev2).
			if c.availableCapsSet().Has(req) {
				enabled = append(enabled, req)
			}
		case imap.CapCondStore:
			// Only enable if server and session support CONDSTORE
			if c.supportsCondStore() {
				enabled = append(enabled, req)
			}
		case imap.CapQResync:
			// Only enable if server and session support QRESYNC
			if c.supportsQResync() {
				enabled = append(enabled, req)
			}
		}
	}

	c.mutex.Lock()
	for _, e := range enabled {
		c.enabled[e] = struct{}{}
	}
	c.mutex.Unlock()

	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("ENABLED")
	for _, c := range enabled {
		enc.SP().Atom(string(c))
	}
	return enc.CRLF()
}
