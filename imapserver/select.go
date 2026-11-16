package imapserver

import (
	"errors"
	"fmt"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func (c *Conn) handleSelect(tag string, dec *imapwire.Decoder, readOnly bool) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) {
		return dec.Err()
	}

	options := imap.SelectOptions{ReadOnly: readOnly}

	if dec.SP() {
		// select-params = SP "(" select-param *(SP select-param) ")" (RFC 4466
		// §3): at least one parameter.
		err := dec.ExpectNonEmptyList(func() error {
			var param string
			if !dec.ExpectAtom(&param) {
				return dec.Err()
			}

			switch imapwire.ToUpperASCII(param) {
			case "CONDSTORE":
				// The parameter is CONDSTORE's (RFC 7162 §3.1.8), and "any
				// parameter not defined by extensions that the server
				// supports must be rejected with a BAD response" (RFC 4466
				// §2.1).
				if !c.supportsCondStore() {
					return newClientBugError("CONDSTORE parameter requires CONDSTORE")
				}
				if options.CondStore {
					return newClientBugError("duplicate CONDSTORE parameter")
				}
				options.CondStore = true
				// SELECT/EXAMINE (CONDSTORE) is a CONDSTORE-enabling command
				// (RFC 7162 §3.1): the client is now CONDSTORE-aware.
				c.markCondStoreEnabled()
			case "QRESYNC":
				// "A server MUST respond with a tagged BAD response if the Quick
				// Resynchronization parameter to the SELECT/EXAMINE command is
				// specified and the client hasn't issued "ENABLE QRESYNC" in the
				// current connection" (RFC 7162 §3.2.5). Refuse it before reading
				// its arguments: returning without reading them accepted a bare
				// "(QRESYNC)".
				if !c.enabledHas(imap.CapQResync) {
					return newClientBugError("QRESYNC parameter requires ENABLE QRESYNC")
				}
				if options.QResync != nil {
					return newClientBugError("duplicate QRESYNC parameter")
				}
				if !dec.ExpectSP() {
					return dec.Err()
				}
				qresyncData, err := readQResyncParam(dec)
				if err != nil {
					return err
				}
				options.QResync = qresyncData
			default:
				return newClientBugError(fmt.Sprintf("unknown SELECT parameter: %v", param))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	wasCondStore := c.CondStoreEnabled()
	rollbackOnBad := func(err error) error {
		var imapErr *imap.Error
		if !wasCondStore && errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeBad {
			c.rollbackCondStore()
		}
		return err
	}

	// The selected mailbox is about to change: no NotifyPoll delivery may be in
	// flight across the switch, or an update queued for the old mailbox would
	// reach the client while the new one is selected — an EXISTS or EXPUNGE
	// that then means something entirely different (RFC 5465 §6.1: the SELECTED
	// specifiers refer to "the mailbox that was selected using either SELECT or
	// EXAMINE"). The pump is restarted once the whole SELECT response has been
	// written: this defer is registered before the response encoder is opened,
	// so it runs after the encoder is closed.
	defer c.restartNotifyPump(c.fenceNotifyPump())

	if c.state == imap.ConnStateSelected {
		if err := c.flushClosingCondStore(); err != nil {
			return err
		}
		c.commitCondStore()
		if err := c.session.Unselect(c.ctx); err != nil {
			return rollbackOnBad(err)
		}
		c.state = imap.ConnStateAuthenticated
		c.selectedReadOnly = false
		c.selectedPrivateStore = false
		err := c.writeStatusResp("", &imap.StatusResponse{
			Type: imap.StatusResponseTypeOK,
			Code: "CLOSED",
			Text: "Previous mailbox is now closed",
		})
		if err != nil {
			return err
		}
	} else {
		c.commitCondStore()
	}

	data, err := c.session.Select(c.ctx, mailbox, &options)
	if err != nil {
		return rollbackOnBad(err)
	}

	enc := newResponseEncoder(c)
	defer enc.end()
	enc.tagged = tag != ""

	// RFC 7162 section 3.2.5 opens the mailbox and sends the standard
	// SELECT/EXAMINE responses before processing the resynchronization data.
	writeExists(enc.Encoder, data.NumMessages)
	if !c.isIMAP4rev2() {
		writeObsoleteRecent(enc.Encoder, data.NumRecent)
		if data.FirstUnseenSeqNum != 0 {
			writeObsoleteUnseen(enc.Encoder, data.FirstUnseenSeqNum)
		}
	}

	writeUIDValidity(enc.Encoder, data.UIDValidity)
	writeUIDNext(enc.Encoder, data.UIDNext)
	writeFlags(enc.Encoder, data.Flags)
	writePermanentFlags(enc.Encoder, data.PermanentFlags)
	if data.List != nil {
		// Write into the encoder this function already holds: opening a second
		// one would deadlock on the non-reentrant encoder mutex.
		if err := writeListData(enc.Encoder, data.List, nil, false); err != nil {
			return err
		}
	}

	// CondStore could be disabled for this client or not enabled in session
	condStoreSupported := c.supportsCondStore()
	if condStoreSupported {
		if data.HighestModSeq > 0 {
			writeHighestModSeq(enc.Encoder, data.HighestModSeq)
		} else {
			writeNoModSeq(enc.Encoder)
		}
	}

	// After the opening responses, report historical expunges before modified
	// messages (RFC 7162 sections 3.2.5.1 and 3.2.6).
	if len(data.Vanished) > 0 {
		if err := writeVanishedEarlier(enc.Encoder, data.Vanished); err != nil {
			return err
		}
	}

	for _, mod := range data.Modified {
		if err := writeQResyncFetch(enc.Encoder, mod); err != nil {
			return err
		}
	}

	c.state = imap.ConnStateSelected
	// Only a NOMODSEQ response actually sent for this mailbox exempts the
	// first enabling command; omitted data while CONDSTORE was filtered out
	// says nothing about whether a later enabling command needs the response.
	c.selectedNoModSeq = condStoreSupported && data.HighestModSeq == 0
	c.selectedReadOnly = readOnly || data.ReadOnly
	c.selectedPrivateStore = !readOnly && data.ReadOnly && len(data.PermanentFlags) > 0

	var (
		cmdName string
		code    imap.ResponseCode
	)
	if readOnly {
		cmdName = "EXAMINE"
	} else {
		cmdName = "SELECT"
	}
	// RFC 4314 §5.2: SELECT also returns READ-ONLY when the session reports the user
	// lacks the rights to modify the mailbox (data.ReadOnly). EXAMINE is always
	// READ-ONLY.
	if readOnly || data.ReadOnly {
		code = "READ-ONLY"
	} else {
		code = "READ-WRITE"
	}
	return writeStatusResp(enc.Encoder, tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Code: code,
		Text: fmt.Sprintf("%v completed", cmdName),
	})
}

// readQResyncParam reads the value of the QRESYNC select parameter (RFC 7162
// §7):
//
//	select-param        =/  "QRESYNC" SP "(" uidvalidity SP
//	                    mod-sequence-value [SP known-uids]
//	                    [SP seq-match-data] ")"
//	seq-match-data      =  "(" known-sequence-set SP known-uid-set ")"
//
// Both parenthesized groups hold their fields exactly once: they are not
// lists. Read as lists, "()" was taken for no parameter at all, so "(QRESYNC
// ())" selected the mailbox without resynchronization, and a repeated group
// replaced the first.
func readQResyncParam(dec *imapwire.Decoder) (*imap.QResyncData, error) {
	var uidValidity uint32
	var modSeq uint64
	if !dec.ExpectSpecial('(') || !dec.ExpectNumber(&uidValidity) || !dec.ExpectSP() || !dec.ExpectModSequenceValue(&modSeq) {
		return nil, dec.Err()
	}
	// uidvalidity = nz-number (RFC 7162 Section 7).
	if uidValidity == 0 {
		return nil, &imapwire.DecoderExpectError{Message: "expected uidvalidity, got 0"}
	}

	qresyncData := &imap.QResyncData{
		UIDValidity: uidValidity,
		ModSeq:      modSeq,
	}

	// RFC 7162 §3.2.5: "[SP known-uids] [SP seq-match-data]". Either may be
	// absent, and known-uids is a sequence set where seq-match-data is
	// parenthesised, so tell them apart by the next byte. Attempting the
	// sequence set and continuing on failure records a decode error, and the
	// decoder stops reading input once one is recorded -- so a client that
	// sent seq-match-data alone lost the rest of its command.
	if dec.SP() {
		seqMatch := dec.NextByteIs('(')
		if !seqMatch {
			var knownUIDs imap.UIDSet
			if !dec.ExpectUIDSet(&knownUIDs) {
				return nil, dec.Err()
			}
			if err := refuseQResyncStar(knownUIDs, "known-uids"); err != nil {
				return nil, err
			}
			qresyncData.KnownUIDs = knownUIDs
			seqMatch = dec.SP()
		}

		if seqMatch {
			// seq-match-data is "known-sequence-set SP known-uid-set": the
			// first half is sequence numbers, not UIDs.
			var seqNums imap.SeqSet
			var uids imap.UIDSet
			if !dec.ExpectSpecial('(') || !dec.ExpectSeqSet(&seqNums) || !dec.ExpectSP() || !dec.ExpectUIDSet(&uids) || !dec.ExpectSpecial(')') {
				return nil, dec.Err()
			}
			if err := refuseQResyncStar(seqNums, "known-sequence-set"); err != nil {
				return nil, err
			}
			if err := refuseQResyncStar(uids, "known-uid-set"); err != nil {
				return nil, err
			}
			qresyncData.SeqMatch = &imap.QResyncSeqMatch{
				SeqNums: seqNums,
				UIDs:    uids,
			}
		}
	}

	if !dec.ExpectSpecial(')') {
		return nil, dec.Err()
	}
	return qresyncData, nil
}

// refuseQResyncStar refuses a set naming "*" ("*" or "n:*"): known-uids,
// known-sequence-set and known-uid-set are sequence-sets in which "*" is not
// allowed (RFC 7162 Section 7). The backend would get "*" as 0, and a known-uids
// of "*" then came back as "VANISHED (EARLIER) *". "$" (RFC 5182) is not a "*".
func refuseQResyncStar(set imap.NumSet, name string) error {
	if set.Dynamic() && !imap.IsSearchRes(set) {
		return &imapwire.DecoderExpectError{Message: fmt.Sprintf(`"*" is not allowed in %v`, name)}
	}
	return nil
}

func (c *Conn) handleUnselect(dec *imapwire.Decoder, expunge bool) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	// Same fence as SELECT: nothing may be delivered for the mailbox being
	// closed while it is being closed.
	defer c.restartNotifyPump(c.fenceNotifyPump())

	if err := c.flushClosingCondStore(); err != nil {
		return err
	}

	// RFC 3501 §6.4.2: "No messages are removed, and no error is given, if the
	// mailbox is selected by an EXAMINE command or is otherwise selected
	// read-only." Skip the expunge attempt entirely for read-only mailboxes.
	if expunge && !c.selectedReadOnly {
		w := &ExpungeWriter{conn: c}
		if err := c.session.Expunge(c.ctx, w, nil); err != nil {
			// RFC 4314 §4: for CLOSE, if the server cannot expunge because the user
			// lacks the "e" right, it MUST ignore the expunge request, close the
			// mailbox, and return the tagged OK response. Other errors still fail.
			var imapErr *imap.Error
			if !errors.As(err, &imapErr) || imapErr.Code != imap.ResponseCodeNoPerm {
				return err
			}
		}
	}

	if err := c.session.Unselect(c.ctx); err != nil {
		return err
	}

	c.state = imap.ConnStateAuthenticated
	c.selectedReadOnly = false
	c.selectedPrivateStore = false
	return nil
}

func writeObsoleteUnseen(enc *imapwire.Encoder, n uint32) error {
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("UNSEEN").SP().Number(n).Special(']')
	enc.SP().Text("First unseen message")
	return enc.CRLF()
}

func writeUIDValidity(enc *imapwire.Encoder, uidValidity uint32) error {
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("UIDVALIDITY").SP().Number(uidValidity).Special(']')
	enc.SP().Text("UIDs valid")
	return enc.CRLF()
}

func writeUIDNext(enc *imapwire.Encoder, uidNext imap.UID) error {
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("UIDNEXT").SP().UID(uidNext).Special(']')
	enc.SP().Text("Predicted next UID")
	return enc.CRLF()
}

func (c *Conn) writeFlags(flags []imap.Flag) error { // also used by UpdateWriter
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeFlags(enc.Encoder, flags)
}

func writeFlags(enc *imapwire.Encoder, flags []imap.Flag) error {
	enc.Atom("*").SP().Atom("FLAGS").SP().List(len(flags), func(i int) {
		enc.Flag(flags[i])
	})
	return enc.CRLF()
}

func writePermanentFlags(enc *imapwire.Encoder, flags []imap.Flag) error {
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("PERMANENTFLAGS").SP().List(len(flags), func(i int) {
		enc.Flag(flags[i])
	}).Special(']')
	enc.SP().Text("Permanent flags")
	return enc.CRLF()
}

func writeHighestModSeq(enc *imapwire.Encoder, highestModSeq uint64) error {
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("HIGHESTMODSEQ").SP().ModSeq(highestModSeq).Special(']')
	enc.SP().Text("Highest modification sequence")
	return enc.CRLF()
}

func writeNoModSeq(enc *imapwire.Encoder) error {
	enc.Atom("*").SP().Atom("OK").SP()
	enc.Special('[').Atom("NOMODSEQ").Special(']')
	enc.SP().Text("Mailbox does not support modification sequences")
	return enc.CRLF()
}

func writeVanishedEarlier(enc *imapwire.Encoder, uids imap.UIDSet) error {
	enc.Atom("*").SP().Atom("VANISHED").SP()
	enc.Special('(').Atom("EARLIER").Special(')')
	enc.SP().NumSet(uids)
	return enc.CRLF()
}

func writeQResyncFetch(enc *imapwire.Encoder, mod imap.SelectModifiedData) error {
	enc.Atom("*").SP().Number(mod.SeqNum).SP().Atom("FETCH").SP().Special('(')
	enc.Atom("UID").SP().UID(mod.UID)
	enc.SP().Atom("FLAGS").SP().List(len(mod.Flags), func(i int) {
		enc.Flag(mod.Flags[i])
	})
	enc.SP().Atom("MODSEQ").SP().Special('(').ModSeq(mod.ModSeq).Special(')')
	enc.Special(')')
	return enc.CRLF()
}
