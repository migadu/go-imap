package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleStore(dec *imapwire.Decoder, numKind NumKind) error {
	var (
		numSet imap.NumSet
		item   string
	)
	if !dec.ExpectSP() || !dec.ExpectNumSet(numKind.wire(), &numSet) || !dec.ExpectSP() {
		return dec.Err()
	}

	options := imap.StoreOptions{UIDStore: numKind == NumKindUID}
	if dec.Special('(') {
		var param string
		if !dec.ExpectAtom(&param) {
			return dec.Err()
		}

		if strings.ToUpper(param) == "UNCHANGEDSINCE" {
			if !dec.ExpectSP() || !dec.ExpectModSeq(&options.UnchangedSince) {
				return dec.Err()
			}
			// A modifier the server does not support for this client must be
			// rejected with a tagged BAD (RFC 4466 §2.5). Silently dropping it
			// instead would invert the request's meaning: an explicit
			// "UNCHANGEDSINCE 0" is the always-fail probe (RFC 7162 §3.1.3.1),
			// and running it as an unconditional store would modify exactly the
			// messages the client asked never to touch.
			if !c.supportsCondStore() {
				return newClientBugError("UNCHANGEDSINCE requires CONDSTORE")
			}
			// Record presence separately from the value: RFC 7162 §3.1.3.1 gives
			// an absent modifier (unconditional store) and an explicit
			// "UNCHANGEDSINCE 0" (always-fail probe) opposite meanings, and the
			// value alone cannot tell them apart.
			options.UnchangedSinceSet = true
			// STORE ... (UNCHANGEDSINCE n) is a CONDSTORE-enabling command
			// (RFC 7162 §3.1), including for n = 0.
			c.markCondStoreEnabled()
		} else {
			return newClientBugError(fmt.Sprintf("unknown STORE modifier: %v", param))
		}
		if !dec.ExpectSpecial(')') || !dec.ExpectSP() {
			return dec.Err()
		}
	}

	if !dec.ExpectAtom(&item) || !dec.ExpectSP() {
		return dec.Err()
	}
	var flags []imap.Flag
	isList, err := dec.List(func() error {
		flag, err := internal.ExpectFlag(dec)
		if err != nil {
			return err
		}
		flags = append(flags, flag)
		return nil
	})
	if err != nil {
		return err
	} else if !isList {
		for {
			flag, err := internal.ExpectFlag(dec)
			if err != nil {
				return err
			}
			flags = append(flags, flag)

			if !dec.SP() {
				break
			}
		}
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	item = strings.ToUpper(item)
	silent := strings.HasSuffix(item, ".SILENT")
	item = strings.TrimSuffix(item, ".SILENT")

	var op imap.StoreFlagsOp
	switch {
	case strings.HasPrefix(item, "+"):
		op = imap.StoreFlagsAdd
		item = strings.TrimPrefix(item, "+")
	case strings.HasPrefix(item, "-"):
		op = imap.StoreFlagsDel
		item = strings.TrimPrefix(item, "-")
	default:
		op = imap.StoreFlagsSet
	}

	if item != "FLAGS" {
		return newClientBugError("STORE can only change FLAGS")
	}

	if err := c.checkWritableMailbox(); err != nil {
		return err
	}

	w := &FetchWriter{conn: c}
	return c.session.Store(c.ctx, w, numSet, &imap.StoreFlags{
		Op:     op,
		Silent: silent,
		Flags:  flags,
	}, &options)
}
