package imap

// SortKey is a sort key for the SORT command.
type SortKey string

const (
	SortKeyArrival     SortKey = "ARRIVAL"
	SortKeyCc          SortKey = "CC"
	SortKeyDate        SortKey = "DATE"
	SortKeyDisplayFrom SortKey = "DISPLAYFROM"
	SortKeyDisplayTo   SortKey = "DISPLAYTO"
	SortKeyFrom        SortKey = "FROM"
	SortKeySize        SortKey = "SIZE"
	SortKeySubject     SortKey = "SUBJECT"
	SortKeyTo          SortKey = "TO"
)

// SortCriterion is a sort criterion for the SORT command.
type SortCriterion struct {
	Key     SortKey
	Reverse bool
}

// SortOptions contains options for the SORT command.
type SortOptions struct {
	// Requires ESORT extension
	ReturnMin   bool
	ReturnMax   bool
	ReturnAll   bool
	ReturnCount bool
}

// SortData is the data returned by a SORT command.
type SortData struct {
	// All lists the matching messages in sort order.
	All []uint32
	// Min and Max are the first and last messages in sort order, not the
	// lowest and highest numbers (RFC 5267 §3.1: the "lowest sorted" and
	// "highest sorted" messages).
	Min   uint32
	Max   uint32
	Count uint32

	// ModSeq is the highest mod-sequence of the messages the response returns,
	// or zero if there are none. Following RFC 4731 §3.2, which RFC 7162
	// §3.1.10 applies to SORT, that depends on the return options:
	//
	//   - MIN alone: the Min message's mod-sequence
	//   - MAX alone: the Max message's
	//   - MIN and MAX, without ALL or COUNT: the higher of those two
	//   - otherwise, including a plain SORT: the highest of all matches
	//
	// The server sends it only when the search criteria use MODSEQ (RFC 7162
	// §3.1.9, §3.1.10), so a session may always set it.
	ModSeq uint64 // requires CONDSTORE
}
