package imapclient

import (
	"fmt"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal"
	"github.com/migadu/go-imap/internal/imapwire"
)

// Capability sends a CAPABILITY command.
func (c *Client) Capability() *CapabilityCommand {
	cmd := &CapabilityCommand{}
	c.beginCommand("CAPABILITY", cmd).end()
	return cmd
}

func (c *Client) handleCapability() error {
	caps, err := readCapabilities(c.dec)
	if err != nil {
		return err
	}
	c.setCaps(caps)
	if cmd := findPendingCmdByType[*CapabilityCommand](c); cmd != nil {
		cmd.caps = caps
	}

	// Workaround: some servers (Gmail, older Dovecot) send the capabilities
	// of the authenticated state as an untagged response ahead of LOGIN's or
	// AUTHENTICATE's tagged OK, where RFC 9051 section 6.2.3 has a CAPABILITY
	// response code in that OK. With the command the oldest one in flight,
	// the response answers no CAPABILITY command sent before it, so it is
	// remembered as the command's. See:
	// https://github.com/emersion/go-imap/issues/561
	c.mutex.Lock()
	var oldest command
	if len(c.pendingCmds) > 0 {
		oldest = c.pendingCmds[0]
	}
	c.mutex.Unlock()
	switch cmd := oldest.(type) {
	case *loginCommand:
		cmd.caps = caps
	case *authenticateCommand:
		cmd.caps = caps
	}
	return nil
}

// CapabilityCommand is a CAPABILITY command.
type CapabilityCommand struct {
	commandBase
	caps imap.CapSet
	// refresh is set on a command the client sends on its own; see
	// Client.resetCaps.
	refresh *capsRefresh
}

func (cmd *CapabilityCommand) Wait() (imap.CapSet, error) {
	err := cmd.wait()
	return cmd.caps, err
}

func readCapabilities(dec *imapwire.Decoder) (imap.CapSet, error) {
	caps := make(imap.CapSet)
	// capability-data has literal SP separators. Decoder.SP also tolerates
	// omitted spaces before lists, which cannot occur in a capability list.
	for dec.Special(' ') {
		// Some IMAP servers send multiple SP between caps:
		// https://github.com/emersion/go-imap/pull/652
		for dec.Special(' ') {
		}
		// Tolerate trailing spaces in both response codes and untagged lines.
		// Leave the delimiter for the caller to validate and consume, without
		// looking past this response into whatever the server sends next.
		if dec.PeekSpecial(']') || dec.PeekSpecial('\r') || dec.PeekSpecial('\n') {
			break
		}

		cap, err := internal.ExpectCap(dec)
		if err != nil {
			return caps, fmt.Errorf("in capability-data: %w", err)
		}
		caps[cap] = struct{}{}
	}
	return caps, dec.Err()
}
