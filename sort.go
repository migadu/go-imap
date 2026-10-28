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
	All   []uint32
	Min   uint32
	Max   uint32
	Count uint32

	// ModSeq is the highest mod-sequence of the returned messages, or zero if
	// none are returned. The server sends it only when the search criteria use
	// MODSEQ (RFC 7162 §3.1.9, §3.1.10), so a session may always set it.
	ModSeq uint64 // requires CONDSTORE
}
