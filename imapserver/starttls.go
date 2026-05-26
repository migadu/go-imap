package imapserver

import (
	"crypto/tls"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

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

	err := writeStatusResp(enc.Encoder, tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "Begin TLS negotiation now",
	})
	if err != nil {
		return err
	}

	tlsConn := tls.Server(c.conn, c.server.options.TLSConfig)

	c.mutex.Lock()
	c.conn = tlsConn
	c.mutex.Unlock()

	rw := c.server.options.wrapReadWriter(tlsConn)
	c.br.Reset(rw)
	c.bw.Reset(rw)

	return nil
}
