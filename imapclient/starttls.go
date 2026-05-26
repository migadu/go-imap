package imapclient

import (
	"bufio"
	"crypto/tls"
	"fmt"
)

// startTLS sends a STARTTLS command.
//
// Unlike other commands, this method blocks until the command completes.
func (c *Client) startTLS(config *tls.Config) error {
	upgradeDone := make(chan struct{})
	cmd := &startTLSCommand{
		tlsConfig:   config,
		upgradeDone: upgradeDone,
	}
	enc := c.beginCommand("STARTTLS", cmd)
	enc.flush()
	defer enc.end()

	// Once a client issues a STARTTLS command, it MUST NOT issue further
	// commands until a server response is seen and the TLS negotiation is
	// complete

	if err := cmd.wait(); err != nil {
		return err
	}

	// The decoder goroutine will invoke Client.upgradeStartTLS
	<-upgradeDone

	return cmd.tlsConn.Handshake()
}

// upgradeStartTLS finishes the STARTTLS upgrade after the server has sent an
// OK response. It runs in the decoder goroutine.
func (c *Client) upgradeStartTLS(startTLS *startTLSCommand) {
	defer close(startTLS.upgradeDone)

	// Refuse STARTTLS if server sent buffered data before the OK response.
	// This is the canonical defense against smuggling attacks.
	if c.br.Buffered() > 0 {
		startTLS.err = fmt.Errorf("STARTTLS refused: server sent buffered data before TLS")
		return
	}

	tlsConn := tls.Client(c.conn, startTLS.tlsConfig)
	rw := c.options.wrapReadWriter(tlsConn)

	c.br.Reset(rw)
	// Unfortunately we can't re-use the bufio.Writer here, it races with
	// Client.StartTLS
	c.bw = bufio.NewWriter(rw)

	startTLS.tlsConn = tlsConn
}

type startTLSCommand struct {
	commandBase
	tlsConfig *tls.Config

	upgradeDone chan<- struct{}
	tlsConn     *tls.Conn
}
