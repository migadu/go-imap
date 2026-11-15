package imapserver

import (
	"fmt"
	"strings"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func (c *Conn) handleStatus(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return dec.Err()
	}

	var options imap.StatusOptions
	err := dec.ExpectList(func() error {
		err := readStatusItem(c, dec, &options)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	// RECENT was removed in IMAP4rev2 (RFC 9051). Reject STATUS (RECENT) when the
	// session has ENABLEd IMAP4rev2, or when the server does not advertise
	// IMAP4rev1 at all. This mirrors SELECT, which suppresses the obsolete RECENT
	// response under the same condition (select.go): gating on the session-enabled
	// revision rather than only the server-advertised one, so a rev2 client on a
	// dual-advertised server can't still pull the obsolete item.
	//
	// The state check runs first so a client in the wrong state gets a state
	// error rather than "Unknown STATUS data item".
	if options.NumRecent && c.isIMAP4rev2() {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unknown STATUS data item",
		}
	}

	data, err := c.session.Status(c.ctx, mailbox, &options)
	if err != nil {
		return err
	}

	return c.writeStatus(data, &options)
}

func (c *Conn) writeStatus(data *imap.StatusData, options *imap.StatusOptions) error {
	if err := checkStatusData(data, options); err != nil {
		return err
	}

	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("STATUS").SP().Mailbox(data.Mailbox).SP()
	listEnc := enc.BeginList()
	if options.NumMessages {
		listEnc.Item().Atom("MESSAGES").SP().Number(*data.NumMessages)
	}
	if options.UIDNext {
		listEnc.Item().Atom("UIDNEXT").SP().UID(data.UIDNext)
	}
	if options.UIDValidity {
		listEnc.Item().Atom("UIDVALIDITY").SP().Number(data.UIDValidity)
	}
	if options.NumUnseen {
		listEnc.Item().Atom("UNSEEN").SP().Number(*data.NumUnseen)
	}
	if options.NumDeleted {
		listEnc.Item().Atom("DELETED").SP().Number(*data.NumDeleted)
	}
	if options.Size {
		listEnc.Item().Atom("SIZE").SP().Number64(*data.Size)
	}
	if options.AppendLimit {
		listEnc.Item().Atom("APPENDLIMIT").SP()
		if data.AppendLimit != nil {
			enc.Number(*data.AppendLimit)
		} else {
			enc.NIL()
		}
	}
	if options.DeletedStorage {
		listEnc.Item().Atom("DELETED-STORAGE").SP().Number64(*data.DeletedStorage)
	}
	if options.NumRecent {
		listEnc.Item().Atom("RECENT").SP().Number(*data.NumRecent)
	}
	if options.HighestModSeq {
		listEnc.Item().Atom("HIGHESTMODSEQ").SP().ModSeq(data.HighestModSeq)
	}
	listEnc.End()

	return enc.CRLF()
}

// checkStatusData reports a requested STATUS data item the backend left unset.
// The STATUS response returns "the requested mailbox status information" (RFC
// 9051 §7.3.3), so an unset item is a backend bug: it fails the command rather
// than panicking or leaving the item out. The check runs before anything is
// written, as an error after part of the STATUS line is out would leave the
// line unterminated, with the tagged response glued to it.
func checkStatusData(data *imap.StatusData, options *imap.StatusOptions) error {
	for _, item := range []struct {
		requested, unset bool
		field            string
	}{
		{options.NumMessages, data.NumMessages == nil, "NumMessages"},
		{options.NumUnseen, data.NumUnseen == nil, "NumUnseen"},
		{options.NumDeleted, data.NumDeleted == nil, "NumDeleted"},
		{options.Size, data.Size == nil, "Size"},
		{options.DeletedStorage, data.DeletedStorage == nil, "DeletedStorage"},
		{options.NumRecent, data.NumRecent == nil, "NumRecent"},
	} {
		if item.requested && item.unset {
			return fmt.Errorf("imapserver: backend did not populate %v in StatusData", item.field)
		}
	}
	return nil
}

func readStatusItem(c *Conn, dec *imapwire.Decoder, options *imap.StatusOptions) error {
	var name string
	if !dec.ExpectAtom(&name) {
		return dec.Err()
	}
	switch strings.ToUpper(name) {
	case "MESSAGES":
		options.NumMessages = true
	case "UIDNEXT":
		options.UIDNext = true
	case "UIDVALIDITY":
		options.UIDValidity = true
	case "UNSEEN":
		options.NumUnseen = true
	case "DELETED":
		options.NumDeleted = true
	case "SIZE":
		options.Size = true
	case "APPENDLIMIT":
		options.AppendLimit = true
	case "DELETED-STORAGE":
		options.DeletedStorage = true
	case "RECENT":
		options.NumRecent = true
	case "HIGHESTMODSEQ":
		// The status data item is CONDSTORE's (RFC 7162 §3.1.7).
		if !c.supportsCondStore() {
			return newClientBugError("HIGHESTMODSEQ requires CONDSTORE")
		}
		options.HighestModSeq = true
	default:
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unknown STATUS data item",
		}
	}
	return nil
}
