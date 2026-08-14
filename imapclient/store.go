package imapclient

import (
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// Store sends a STORE command.
//
// Unless StoreFlags.Silent is set, the server will return the updated values.
//
// A nil options pointer is equivalent to a zero options value.
func (c *Client) Store(numSet imap.NumSet, store *imap.StoreFlags, options *imap.StoreOptions) *FetchCommand {
	cmd := &FetchCommand{
		numSet: numSet,
		msgs:   make(chan *FetchMessageData, 128),
	}
	enc := c.beginCommand(uidCmdName("STORE", imapwire.NumSetKind(numSet)), cmd)
	enc.SP().NumSet(numSet).SP()
	// An explicit "UNCHANGEDSINCE 0" is the always-fail probe of RFC 7162
	// §3.1.3.1 and must reach the server, so emit the modifier whenever it is
	// marked present. A non-zero value implies presence, for callers written
	// before StoreOptions.UnchangedSinceSet existed.
	if options != nil && (options.UnchangedSinceSet || options.UnchangedSince != 0) {
		enc.Special('(').Atom("UNCHANGEDSINCE").SP().ModSeq(options.UnchangedSince).Special(')').SP()
	}
	switch store.Op {
	case imap.StoreFlagsSet:
		// nothing to do
	case imap.StoreFlagsAdd:
		enc.Special('+')
	case imap.StoreFlagsDel:
		enc.Special('-')
	default:
		panic(fmt.Errorf("imapclient: unknown store flags op: %v", store.Op))
	}
	enc.Atom("FLAGS")
	if store.Silent {
		enc.Atom(".SILENT")
	}
	enc.SP().List(len(store.Flags), func(i int) {
		enc.Flag(store.Flags[i])
	})
	enc.end()
	return cmd
}

// readRespCodeModified parses the number set carried by a MODIFIED response
// code (RFC 7162 §3.1.3). The set is interpreted in the number space of the
// STORE command that produced it: UIDs for UID STORE, sequence numbers for
// STORE. The pending command carries that number space via its numSet.
func readRespCodeModified(dec *imapwire.Decoder, cmd command) (imap.NumSet, error) {
	// MODIFIED is only emitted for STORE / UID STORE, which are backed by a
	// FetchCommand. Default to sequence numbers for any other (unexpected)
	// command so the decoder still consumes the set and stays in sync.
	kind := imapwire.NumKindSeq
	if fetchCmd, ok := cmd.(*FetchCommand); ok {
		kind = imapwire.NumSetKind(fetchCmd.numSet)
	}
	var modified imap.NumSet
	if !dec.ExpectNumSet(kind, &modified) {
		return nil, dec.Err()
	}
	return modified, nil
}

// Modified returns the messages that a conditional STORE did not update because
// they failed the UNCHANGEDSINCE precondition (RFC 7162 §3.1.3).
//
// The server reports these messages in a MODIFIED response code on the tagged
// completion line. The returned set uses the command's number space: sequence
// numbers when Store was called with a SeqSet, UIDs when called with a UIDSet.
//
// Modified is only populated for a conditional STORE issued with
// StoreOptions.UnchangedSince, and only once the command has completed (e.g.
// after Close or Collect returns). It is nil for a regular FETCH, an
// unconditional STORE, or a conditional STORE in which every message satisfied
// the precondition. In the expunged-message case the server reports MODIFIED on
// a NO completion: Modified is still populated, and Close/Collect return the
// corresponding *imap.Error.
func (cmd *FetchCommand) Modified() imap.NumSet {
	return cmd.modified
}
