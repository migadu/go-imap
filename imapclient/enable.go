package imapclient

import (
	"fmt"

	"github.com/migadu/go-imap"
)

// Enable sends an ENABLE command.
//
// This command requires support for IMAP4rev2 or the ENABLE extension.
func (c *Client) Enable(caps ...imap.Cap) *EnableCommand {
	// Enabling an extension may change the IMAP syntax, so only allow the
	// extensions we support here
	for _, name := range caps {
		switch name {
		case imap.CapIMAP4rev2, imap.CapUTF8Accept, imap.CapMetadata, imap.CapMetadataServer, imap.CapQResync, imap.CapCondStore:
			// ok
		default:
			done := make(chan struct{})
			close(done)
			err := fmt.Errorf("imapclient: cannot enable %q: not supported", name)
			return &EnableCommand{commandBase: commandBase{done: done, err: err}}
		}
	}

	cmd := &EnableCommand{requested: make(imap.CapSet)}
	for _, name := range caps {
		cmd.requested[name] = struct{}{}
	}
	enc := c.beginCommand("ENABLE", cmd)
	for _, c := range caps {
		enc.SP().Atom(string(c))
	}
	enc.end()
	return cmd
}

func (c *Client) handleEnabled() error {
	caps, err := readCapabilities(c.dec)
	if err != nil {
		return err
	}

	// RFC 9051 §7.2.1: "The ENABLED response occurs as a result of an ENABLE
	// command." One that answers none, or comes before authentication, where
	// ENABLE is not valid (§6.3.1, §11.3), enables nothing.
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.state != imap.ConnStateAuthenticated && c.state != imap.ConnStateSelected {
		return nil
	}

	// ENABLED is untagged. Match its extensions to the requests rather than
	// assuming that pipelined ENABLE commands complete in send order.
	var pending []*EnableCommand
	for _, cmd := range c.pendingCmds {
		if cmd, ok := cmd.(*EnableCommand); ok {
			pending = append(pending, cmd)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if pending[0].data.Caps == nil {
		pending[0].data.Caps = make(imap.CapSet)
	}
	previouslyEnabled := c.enabled.Copy()
	for name := range caps {
		matched := false
		for _, cmd := range pending {
			if cmd.requests(name) {
				if cmd.data.Caps == nil {
					cmd.data.Caps = make(imap.CapSet)
				}
				cmd.data.Caps[name] = struct{}{}
				matched = true
			}
		}
		if !matched {
			// A server may echo capabilities enabled by an earlier ENABLE
			// (RFC 9051 section 6.3.1). That enables nothing new. Unrequested
			// new capabilities must never change the client's wire syntax.
			if !previouslyEnabled.Has(name) {
				continue
			}
			pending[0].data.Caps[name] = struct{}{}
		}
		c.enabled[name] = struct{}{}
	}
	c.dec.QuotedUTF8 = c.useQuotedUTF8Locked()
	return nil
}

// EnableCommand is an ENABLE command.
type EnableCommand struct {
	commandBase
	requested imap.CapSet
	data      EnableData
}

func (cmd *EnableCommand) requests(name imap.Cap) bool {
	if _, ok := cmd.requested[name]; ok {
		return true
	}
	// Enabling QRESYNC also enables CONDSTORE (RFC 7162 section 3.2).
	_, qresync := cmd.requested[imap.CapQResync]
	return name == imap.CapCondStore && qresync
}

func (cmd *EnableCommand) Wait() (*EnableData, error) {
	return &cmd.data, cmd.wait()
}

// EnableData is the data returned by the ENABLE command.
type EnableData struct {
	// Capabilities that were successfully enabled
	Caps imap.CapSet
}
