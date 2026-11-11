package imapserver

import (
	"context"
	"crypto/tls"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// An error after the positive STARTTLS response is fatal: the peer now expects
// TLS records, so readCommand must not turn it into an IMAP status response.
type startTLSHandshakeError struct{ err error }

func (e *startTLSHandshakeError) Error() string {
	return "imapserver: STARTTLS handshake failed: " + e.err.Error()
}

func (e *startTLSHandshakeError) Unwrap() error { return e.err }

func (c *Conn) canStartTLS() bool {
	_, isTLS := c.conn.(*tls.Conn)
	return c.server.options.TLSConfig != nil && c.state == imap.ConnStateNotAuthenticated && !isTLS
}

func (c *Conn) handleStartTLS(tag string, dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if c.server.options.TLSConfig == nil {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "STARTTLS not supported",
		}
	}
	if !c.canStartTLS() {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "STARTTLS not available",
		}
	}

	// Refuse STARTTLS if client has buffered data before the command.
	// This is the canonical defense against smuggling attacks.
	if c.br.Buffered() > 0 {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "STARTTLS refused: client buffered data before TLS",
		}
	}

	// Do not allow to write cleartext data past this point: keep c.encMutex
	// locked until the end
	enc := newResponseEncoder(c)
	defer enc.end()
	plain := c.NetConn()
	if h := c.server.options.OnStartTLS; h != nil {
		h(plain)
	}

	err := writeStatusResp(enc.Encoder, tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "Begin TLS negotiation now",
	})
	if err != nil {
		return err
	}

	tlsConn := tls.Server(plain, c.server.options.TLSConfig)

	c.mutex.Lock()
	c.conn = tlsConn
	c.mutex.Unlock()

	// The same recorder as before the upgrade: this is the line that makes a
	// STARTTLS capture plaintext.
	rw := c.server.options.wrapReadWriter(tlsConn, c.debug)
	c.br.Reset(rw)
	c.bw.Reset(rw)

	if h := c.server.options.OnTLSHandshakeComplete; h != nil {
		// Ordinarily the next command read performs the handshake lazily, with
		// a fresh command-read deadline. Complete it here to publish the secure
		// writer even if the first command is coalesced with TLS Finished and
		// the protocol then runs that command without another transport read.
		c.setReadTimeout(cmdReadTimeout)
		ctx, cancel := context.WithTimeout(c.ctx, cmdReadTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return &startTLSHandshakeError{err}
		}
		h(plain, tlsConn)
	}

	return nil
}
