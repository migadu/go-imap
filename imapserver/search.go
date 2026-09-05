package imapserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal"
	"github.com/emersion/go-imap/v2/internal/imapwire"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func (c *Conn) handleSearch(tag string, dec *imapwire.Decoder, numKind NumKind) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}
	var (
		atom     string
		options  imap.SearchOptions
		extended bool
	)
	if maybeReadSearchKeyAtom(dec, &atom) && strings.EqualFold(atom, "RETURN") {
		if err := readSearchReturnOpts(dec, &options); err != nil {
			return fmt.Errorf("in search-return-opts: %w", err)
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		extended = true
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}
	if strings.EqualFold(atom, "CHARSET") {
		var charset string
		if !dec.ExpectSP() || !dec.ExpectAString(&charset) || !dec.ExpectSP() {
			return dec.Err()
		}
		switch strings.ToUpper(charset) {
		case "US-ASCII", "UTF-8":
			// nothing to do
		default:
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeBadCharset, // TODO: return list of supported charsets
				Text: "Only US-ASCII and UTF-8 are supported SEARCH charsets",
			}
		}
		atom = ""
		maybeReadSearchKeyAtom(dec, &atom)
	}

	var criteria imap.SearchCriteria
	for {
		var err error
		if atom != "" {
			err = readSearchKeyWithAtom(c, &criteria, dec, atom)
			atom = ""
		} else {
			err = readSearchKey(c, &criteria, dec)
		}
		if err != nil {
			return fmt.Errorf("in search-key: %w", err)
		}

		if !dec.SP() {
			break
		}
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}

	// If no return option is specified, ALL is assumed
	if !options.ReturnMin && !options.ReturnMax && !options.ReturnAll && !options.ReturnCount {
		options.ReturnAll = true
	}

	data, err := c.session.Search(c.ctx, numKind, &criteria, &options)
	if err != nil {
		return err
	}

	var supportsESEARCH bool
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		supportsESEARCH = sessionCaps.Has(imap.CapESearch) || sessionCaps.Has(imap.CapIMAP4rev2)
	} else {
		supportsESEARCH = c.availableCapsSet().Has(imap.CapESearch) || c.availableCapsSet().Has(imap.CapIMAP4rev2)
	}

	// Use the ESEARCH format when the session supports it AND either the client
	// used the extended syntax (RFC 4731) or it has enabled IMAP4rev2.
	//
	// The IMAP4rev2 half is not an extension of RFC 4731 but a requirement of
	// RFC 9051: §6.4.4 gives SEARCH exactly one untagged response, ESEARCH, and
	// says "if no result option is specified or empty list of options is
	// specified as '()', ALL is assumed"; §7.3.4 describes ESEARCH as the
	// response to "a SEARCH or UID SEARCH command", not to an extended one; and
	// Appendix E item 4 records the change as "SEARCH command now requires to
	// return the ESEARCH response (SEARCH response is now deprecated)". So a
	// plain `SEARCH ALL` on a rev2 session must be answered
	// `* ESEARCH (TAG "…") ALL 1:3`, and answering `* SEARCH 1 2 3` leaves a
	// rev2-only client with no results at all — §6.4.4 tells it to ignore
	// SEARCH responses, so the mismatch is silent rather than a parse error.
	//
	// handleSort already applies exactly this rule to its own response form
	// (sort.go: `c.enabledHas(imap.CapIMAP4rev2) || extended`). This brings the
	// command rev2 actually specifies into line with the extension that copied
	// it.
	//
	// Gating on the ENABLED set, never the advertised one, is what keeps this
	// invisible to IMAP4rev1 clients: a server may advertise IMAP4rev2 alongside
	// IMAP4rev1, and a client that never sent ENABLE keeps receiving the rev1
	// wire form it knows how to parse. supportsESEARCH is kept as the outer
	// guard so the response form can never outrun what the session reports it
	// can do via SessionCapabilities.
	if supportsESEARCH && (extended || c.isIMAP4rev2()) {
		return c.writeESearch(tag, data, &options, numKind)
	} else {
		return c.writeSearch(data.All, data.ModSeq)
	}
}

func (c *Conn) writeESearch(tag string, data *imap.SearchData, options *imap.SearchOptions, numKind NumKind) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("ESEARCH")

	// search-correlator: RFC 7377 §2.1 places MAILBOX and UIDVALIDITY *inside* the
	// same parentheses as TAG (one ESEARCH response per matched mailbox), so the
	// returned UIDs are unambiguous across mailboxes. UIDVALIDITY is REQUIRED
	// whenever MAILBOX is present.
	if tag != "" || data.Mailbox != "" {
		enc.SP().Special('(')
		wrote := false
		if tag != "" {
			enc.Atom("TAG").SP().String(tag)
			wrote = true
		}
		if data.Mailbox != "" {
			if wrote {
				enc.SP()
			}
			enc.Atom("MAILBOX").SP().Mailbox(data.Mailbox)
			enc.SP().Atom("UIDVALIDITY").SP().Number(data.UIDValidity)
		}
		enc.Special(')')
	}

	if numKind == NumKindUID {
		enc.SP().Atom("UID")
	}

	if options.ReturnAll && data.All != nil && !isNumSetEmpty(data.All) {
		enc.SP().Atom("ALL")
		enc.SP().NumSet(data.All)
	}
	if options.ReturnMin && data.Min > 0 {
		enc.SP().Atom("MIN").SP().Number(data.Min)
	}
	if options.ReturnMax && data.Max > 0 {
		enc.SP().Atom("MAX").SP().Number(data.Max)
	}
	if options.ReturnCount {
		enc.SP().Atom("COUNT").SP().Number(data.Count)
	}
	if data.ModSeq > 0 && c.supportsCondStore() {
		enc.SP().Atom("MODSEQ").SP().ModSeq(data.ModSeq)
	}
	return enc.CRLF()
}

func isNumSetEmpty(numSet imap.NumSet) bool {
	switch numSet := numSet.(type) {
	case imap.SeqSet:
		return len(numSet) == 0
	case imap.UIDSet:
		return len(numSet) == 0
	default:
		panic("unknown imap.NumSet type")
	}
}

func (c *Conn) writeSearch(numSet imap.NumSet, modSeq uint64) error {
	enc := newResponseEncoder(c)
	defer enc.end()

	enc.Atom("*").SP().Atom("SEARCH")

	if numSet != nil {
		var ok bool
		switch numSet := numSet.(type) {
		case imap.SeqSet:
			var nums []uint32
			nums, ok = numSet.Nums()
			for _, num := range nums {
				enc.SP().Number(num)
			}
		case imap.UIDSet:
			var uids []imap.UID
			uids, ok = numSet.Nums()
			for _, uid := range uids {
				enc.SP().UID(uid)
			}
		}
		if !ok {
			return fmt.Errorf("imapserver: failed to enumerate message numbers in SEARCH response (dynamic set?)")
		}
	}

	// RFC 7162 §3.4: append the highest mod-sequence of the matched messages to
	// the untagged SEARCH response, e.g. "* SEARCH 2 5 6 (MODSEQ 917162500)".
	// Gated identically to the ESEARCH path (writeESearch).
	if modSeq > 0 && c.supportsCondStore() {
		enc.SP().Special('(').Atom("MODSEQ").SP().ModSeq(modSeq).Special(')')
	}

	return enc.CRLF()
}

func readSearchReturnOpts(dec *imapwire.Decoder, options *imap.SearchOptions) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}
	return dec.ExpectList(func() error {
		var name string
		if !dec.ExpectAtom(&name) {
			return dec.Err()
		}
		switch strings.ToUpper(name) {
		case "MIN":
			options.ReturnMin = true
		case "MAX":
			options.ReturnMax = true
		case "ALL":
			options.ReturnAll = true
		case "COUNT":
			options.ReturnCount = true
		case "SAVE":
			options.ReturnSave = true
		default:
			// RFC 4731: A server MUST ignore any unrecognized return options.
		}
		return nil
	})
}

func maybeReadSearchKeyAtom(dec *imapwire.Decoder, ptr *string) bool {
	return dec.Func(ptr, func(ch byte) bool {
		return ch == '*' || imapwire.IsAtomChar(ch)
	})
}

// maxSearchKeyDepth bounds SEARCH-key nesting. The parenthesized-list path is
// already capped by the decoder's maxListDepth, but NOT/OR recurse directly and
// would otherwise be bounded only by the 50 KiB command size — a single small
// command can drive many thousands of NOT/OR levels. Cap it well above any real
// query.
const maxSearchKeyDepth = 100

func readSearchKey(c *Conn, criteria *imap.SearchCriteria, dec *imapwire.Decoder) error {
	return readSearchKeyDepth(c, criteria, dec, 0)
}

func readSearchKeyDepth(c *Conn, criteria *imap.SearchCriteria, dec *imapwire.Decoder, depth int) error {
	if depth > maxSearchKeyDepth {
		return newClientBugError("SEARCH key nesting too deep")
	}
	var key string
	if maybeReadSearchKeyAtom(dec, &key) {
		return readSearchKeyWithAtomDepth(c, criteria, dec, key, depth)
	}
	return dec.ExpectList(func() error {
		for {
			if err := readSearchKeyDepth(c, criteria, dec, depth+1); err != nil {
				return err
			}
			if !dec.SP() {
				break
			}
		}
		return nil
	})
}

func readSearchKeyWithAtom(c *Conn, criteria *imap.SearchCriteria, dec *imapwire.Decoder, key string) error {
	return readSearchKeyWithAtomDepth(c, criteria, dec, key, 0)
}

func readSearchKeyWithAtomDepth(c *Conn, criteria *imap.SearchCriteria, dec *imapwire.Decoder, key string, depth int) error {
	key = strings.ToUpper(key)
	switch key {
	case "ALL":
		// nothing to do
	case "UID":
		var uidSet imap.UIDSet
		if !dec.ExpectSP() || !dec.ExpectUIDSet(&uidSet) {
			return dec.Err()
		}
		criteria.UID = append(criteria.UID, uidSet)
	case "ANSWERED", "DELETED", "DRAFT", "FLAGGED", "RECENT", "SEEN":
		criteria.Flag = append(criteria.Flag, searchKeyFlag(key))
	case "UNANSWERED", "UNDELETED", "UNDRAFT", "UNFLAGGED", "UNSEEN":
		notKey := strings.TrimPrefix(key, "UN")
		criteria.NotFlag = append(criteria.NotFlag, searchKeyFlag(notKey))
	case "NEW":
		criteria.Flag = append(criteria.Flag, internal.FlagRecent)
		criteria.NotFlag = append(criteria.NotFlag, imap.FlagSeen)
	case "OLD":
		criteria.NotFlag = append(criteria.NotFlag, internal.FlagRecent)
	case "KEYWORD", "UNKEYWORD":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		flag, err := internal.ExpectFlag(dec)
		if err != nil {
			return err
		}
		switch key {
		case "KEYWORD":
			criteria.Flag = append(criteria.Flag, flag)
		case "UNKEYWORD":
			criteria.NotFlag = append(criteria.NotFlag, flag)
		}
	case "BCC", "CC", "FROM", "SUBJECT", "TO":
		var value string
		if !dec.ExpectSP() || !dec.ExpectAString(&value) {
			return dec.Err()
		}
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{
			Key:   cases.Title(language.English).String(strings.ToLower(key)),
			Value: value,
		})
	case "HEADER":
		var key, value string
		if !dec.ExpectSP() || !dec.ExpectAString(&key) || !dec.ExpectSP() || !dec.ExpectAString(&value) {
			return dec.Err()
		}
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{
			Key:   key,
			Value: value,
		})
	case "SINCE", "BEFORE", "ON", "SENTSINCE", "SENTBEFORE", "SENTON":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		t, err := internal.ExpectDate(dec)
		if err != nil {
			return err
		}
		var dateCriteria imap.SearchCriteria
		switch key {
		case "SINCE":
			dateCriteria.Since = t
		case "BEFORE":
			dateCriteria.Before = t
		case "ON":
			dateCriteria.Since = t
			dateCriteria.Before = t.Add(24 * time.Hour)
		case "SENTSINCE":
			dateCriteria.SentSince = t
		case "SENTBEFORE":
			dateCriteria.SentBefore = t
		case "SENTON":
			dateCriteria.SentSince = t
			dateCriteria.SentBefore = t.Add(24 * time.Hour)
		}
		criteria.And(&dateCriteria)
	case "BODY":
		var body string
		if !dec.ExpectSP() || !dec.ExpectAString(&body) {
			return dec.Err()
		}
		criteria.Body = append(criteria.Body, body)
	case "TEXT":
		var text string
		if !dec.ExpectSP() || !dec.ExpectAString(&text) {
			return dec.Err()
		}
		criteria.Text = append(criteria.Text, text)
	case "LARGER", "SMALLER":
		var n int64
		if !dec.ExpectSP() || !dec.ExpectNumber64(&n) {
			return dec.Err()
		}
		switch key {
		case "LARGER":
			criteria.And(&imap.SearchCriteria{Larger: n})
		case "SMALLER":
			criteria.And(&imap.SearchCriteria{Smaller: n})
		}
	case "NOT":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var not imap.SearchCriteria
		if err := readSearchKeyDepth(c, &not, dec, depth+1); err != nil {
			return err
		}
		criteria.Not = append(criteria.Not, not)
	case "OR":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var or [2]imap.SearchCriteria
		if err := readSearchKeyDepth(c, &or[0], dec, depth+1); err != nil {
			return err
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		if err := readSearchKeyDepth(c, &or[1], dec, depth+1); err != nil {
			return err
		}
		criteria.Or = append(criteria.Or, or)
	case "$":
		criteria.UID = append(criteria.UID, imap.SearchRes())
	case "MODSEQ":
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var name string
		var metadataType imap.SearchCriteriaMetadataType
		if dec.Quoted(&name) {
			if !dec.ExpectSP() {
				return dec.Err()
			}
			var typeName string
			if !dec.ExpectAtom(&typeName) || !dec.ExpectSP() {
				return dec.Err()
			}
			metadataType = imap.SearchCriteriaMetadataType(strings.ToLower(typeName))
		}

		var modSeq uint64
		if !dec.ExpectModSeq(&modSeq) {
			return dec.Err()
		}

		// Only apply MODSEQ criteria if CONDSTORE is supported, otherwise ignore
		if c.supportsCondStore() {
			// SEARCH MODSEQ is a CONDSTORE-enabling command (RFC 7162 §3.1).
			c.markCondStoreEnabled()
			criteria.ModSeq = &imap.SearchCriteriaModSeq{
				ModSeq:       modSeq,
				MetadataName: name,
				MetadataType: metadataType,
			}
		}
	default:
		seqSet, err := imapwire.ParseSeqSet(key)
		if err != nil {
			// An unknown key reaches here as a sequence-set candidate; failing
			// to parse it is the client's syntax, not a server fault.
			return &imapwire.DecoderExpectError{
				Message: fmt.Sprintf("invalid search-key %q", key),
			}
		}
		criteria.SeqNum = append(criteria.SeqNum, seqSet)
	}
	return nil
}

func searchKeyFlag(key string) imap.Flag {
	return imap.Flag("\\" + cases.Title(language.English).String(strings.ToLower(key)))
}
