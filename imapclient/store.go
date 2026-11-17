package imapclient

import (
	"fmt"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// Store sends a STORE command.
//
// Unless StoreFlags.Silent is set, the server will return the updated values.
// When numSet is imap.SearchRes(), these responses have the same attribution
// limitations as Fetch with imap.SearchRes().
//
// A nil options pointer is equivalent to a zero options value. The
// UNCHANGEDSINCE modifier (RFC 7162 §3.1.3) is sent when
// options.Conditional() reports it as present; note that
// StoreOptions.UnchangedSinceSet must be set to issue the always-fail probe
// "UNCHANGEDSINCE 0", since a zero UnchangedSince alone means "no modifier".
func (c *Client) Store(numSet imap.NumSet, store *imap.StoreFlags, options *imap.StoreOptions) *FetchCommand {
	cmd := &FetchCommand{
		numSet: numSet,
		msgs:   make(chan *FetchMessageData, 128),
	}
	enc := c.beginCommand(uidCmdName("STORE", imapwire.NumSetKind(numSet)), cmd)
	enc.SP().NumSet(numSet).SP()
	// An explicit "UNCHANGEDSINCE 0" is the always-fail probe of RFC 7162
	// §3.1.3.1 and must reach the server, so emit the modifier whenever it is
	// present — which is what Conditional reports.
	if options.Conditional() {
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
// code (RFC 7162 §3.1.3) without poisoning dec on syntax errors. The set is
// interpreted in the number space of the STORE command that produced it: UIDs
// for UID STORE, sequence numbers for STORE.
func readRespCodeModified(dec *imapwire.Decoder, cmd command) (imap.NumSet, bool) {
	var raw string
	if !dec.SP() || !dec.Func(&raw, func(ch byte) bool { return ch == '*' || imapwire.IsAtomChar(ch) }) || !dec.NextByteIs(']') {
		return nil, false
	}
	if raw == "$" {
		return imap.SearchRes(), true
	}
	kind := imapwire.NumKindSeq
	if fetchCmd, ok := cmd.(*FetchCommand); ok {
		kind = imapwire.NumSetKind(fetchCmd.numSet)
	}
	return imapwire.ParseNumSet(kind, raw)
}

// Modified returns the messages that a conditional STORE did not update because
// they failed the UNCHANGEDSINCE precondition (RFC 7162 §3.1.3).
//
// The server reports these messages in a MODIFIED response code on the tagged
// completion line. The returned set uses the command's number space: sequence
// numbers when Store was called with a SeqSet, UIDs when called with a UIDSet.
//
// Modified is only populated for a conditional STORE — one whose
// StoreOptions carry the UNCHANGEDSINCE modifier, i.e. with UnchangedSince
// non-zero or UnchangedSinceSet true — and only once the command has completed (e.g.
// after Close or Collect returns). It is nil for a regular FETCH, an
// unconditional STORE, or a conditional STORE in which every message satisfied
// the precondition. In the expunged-message case the server reports MODIFIED on
// a NO completion: Modified is still populated, and Close/Collect return the
// corresponding *imap.Error.
//
// The returned numeric set is a copy: mutating it does not disturb the command,
// so a later call reports the same set. A SEARCHRES reference retains its
// special marker identity.
func (cmd *FetchCommand) Modified() imap.NumSet {
	// SEARCHRES is identified by its backing slice, so copying it would turn
	// the marker into an ordinary empty set.
	if cmd.modified != nil && imap.IsSearchRes(cmd.modified) {
		return imap.SearchRes()
	}
	switch set := cmd.modified.(type) {
	case imap.SeqSet:
		return append(imap.SeqSet(nil), set...)
	case imap.UIDSet:
		return append(imap.UIDSet(nil), set...)
	default:
		return cmd.modified
	}
}
