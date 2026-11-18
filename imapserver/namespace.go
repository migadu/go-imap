package imapserver

import (
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func (c *Conn) handleNamespace(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, ok := c.session.(SessionNamespace)
	if !ok {
		return newClientBugError("NAMESPACE is not supported")
	}

	data, err := session.Namespace(c.ctx)
	if err != nil {
		return err
	}

	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("NAMESPACE").SP()
	writeNamespace(enc.Encoder, data.Personal)
	enc.SP()
	writeNamespace(enc.Encoder, data.Other)
	enc.SP()
	writeNamespace(enc.Encoder, data.Shared)
	return enc.CRLF()
}

func writeNamespace(enc *imapwire.Encoder, l []imap.NamespaceDescriptor) {
	if l == nil {
		enc.NIL()
		return
	}

	// namespace is "(" 1*namespace-descr ")": no SP between the descriptors
	// (RFC 9051 §9, RFC 2342 §6), unlike enc.List's "(" x *(SP x) ")".
	enc.Special('(')
	for _, descr := range l {
		enc.Special('(').String(descr.Prefix).SP()
		if descr.Delim == 0 {
			enc.NIL()
		} else {
			enc.Quoted(string(descr.Delim))
		}
		enc.Special(')')
	}
	enc.Special(')')
}
