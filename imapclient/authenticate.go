package imapclient

import (
	"encoding/base64"

	"github.com/emersion/go-sasl"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
)

// Authenticate sends an AUTHENTICATE command.
//
// Unlike other commands, this method blocks until the SASL exchange completes.
func (c *Client) Authenticate(saslClient sasl.Client) error {
	mech, initialResp, err := saslClient.Start()
	if err != nil {
		return err
	}

	// c.Caps may send a CAPABILITY command, so check it before c.beginCommand
	var hasSASLIR bool
	if initialResp != nil {
		hasSASLIR = c.Caps().Has(imap.CapSASLIR)
	}

	cmd := &authenticateCommand{}
	enc := c.beginCommand("AUTHENTICATE", cmd)
	// Register in wire order, after this command owns the encoder. A queued
	// AUTHENTICATE must not consume another command's literal continuation.
	contReq := c.registerContReq(cmd)
	enc.SP().Atom(mech)
	if initialResp != nil && hasSASLIR {
		enc.SP().Atom(internal.EncodeSASL(initialResp))
		initialResp = nil
	}
	enc.flush()
	if enc.locked {
		c.setWriteTimeout(0)
	}
	defer enc.end()

	for {
		challengeStr, err := contReq.Wait()
		if err != nil {
			return cmd.wait()
		}

		if challengeStr == "" && initialResp != nil {
			contReq = c.registerContReq(cmd)
			if err := c.writeSASLResp(initialResp); err != nil {
				return err
			}
			initialResp = nil
			continue
		}

		challenge, err := internal.DecodeSASL(challengeStr)
		if err != nil {
			return c.cancelAuthenticate(cmd, err)
		}

		resp, err := saslClient.Next(challenge)
		if err != nil {
			return c.cancelAuthenticate(cmd, err)
		}

		contReq = c.registerContReq(cmd)
		if err := c.writeSASLResp(resp); err != nil {
			return err
		}
	}
}

type authenticateCommand struct {
	commandBase
	// caps holds the capabilities the server sent while the command was the
	// oldest one in flight; see Client.handleCapability.
	caps imap.CapSet
}

func (c *Client) cancelAuthenticate(cmd *authenticateCommand, cause error) error {
	if _, completed := cmd.poll(); !completed {
		if err := c.writeSASLLine("*"); err == nil {
			_ = cmd.wait()
		}
	}
	return cause
}

func (c *Client) writeSASLResp(resp []byte) error {
	// A continuation response is plain base64, so a zero-length response is an
	// empty line. The "=" zero-length marker of RFC 4959 Section 3 is valid only
	// in the initial-response slot of the AUTHENTICATE command line: here it is
	// not valid base64, and strict servers (e.g. Dovecot) reject it. Multi-step
	// mechanisms hit this — SASL GSSAPI (RFC 4752) acknowledges the server's
	// AP-REP with an empty token.
	return c.writeSASLLine(base64.StdEncoding.EncodeToString(resp))
}

func (c *Client) writeSASLLine(line string) error {
	c.setWriteTimeout(cmdWriteTimeout)
	defer c.setWriteTimeout(0)
	if _, err := c.bw.WriteString(line + "\r\n"); err != nil {
		c.closeWithError(err)
		return err
	}
	if err := c.bw.Flush(); err != nil {
		c.closeWithError(err)
		return err
	}
	return nil
}

// Unauthenticate sends an UNAUTHENTICATE command.
//
// This command requires support for the UNAUTHENTICATE extension.
func (c *Client) Unauthenticate() *Command {
	cmd := &unauthenticateCommand{}
	c.beginCommand("UNAUTHENTICATE", cmd).end()
	return &cmd.Command
}

type unauthenticateCommand struct {
	Command
}
