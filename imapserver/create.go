package imapserver

import (
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
	"github.com/migadu/go-imap/internal/imapwire"
)

func (c *Conn) handleCreate(dec *imapwire.Decoder) error {
	var (
		name    string
		options imap.CreateOptions
		use     bool
	)
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) {
		return dec.Err()
	}
	if dec.SP() {
		var name string
		if !dec.ExpectSpecial('(') || !dec.ExpectAtom(&name) || !dec.ExpectSP() {
			return dec.Err()
		}
		switch imapwire.ToUpperASCII(name) {
		case "USE":
			use = true
			var err error
			options.SpecialUse, err = internal.ExpectMailboxAttrList(dec)
			if err != nil {
				return err
			}
		default:
			return newClientBugError("unknown CREATE parameter")
		}
		if !dec.ExpectSpecial(')') {
			return dec.Err()
		}
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	// RFC 4466 §2.2: "Any parameter not defined by extensions that the server
	// supports must be rejected with a BAD response." USE is
	// CREATE-SPECIAL-USE's (RFC 6154 §3). The session cannot refuse "USE ()"
	// itself: an empty attribute list reaches it as no USE parameter.
	if use && !c.availableCapsSet().Has(imap.CapCreateSpecialUse) {
		return newClientBugError("CREATE-SPECIAL-USE is not supported")
	}
	return c.session.Create(c.ctx, name, &options)
}
