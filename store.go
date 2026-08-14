package imap

// StoreOptions contains options for the STORE command.
type StoreOptions struct {
	// UnchangedSince is the value of the UNCHANGEDSINCE modifier
	// (RFC 7162 §3.1.3.1). Requires CONDSTORE.
	//
	// The value 0 is only meaningful together with UnchangedSinceSet: see the
	// note there.
	UnchangedSince uint64

	// UnchangedSinceSet reports whether the UNCHANGEDSINCE modifier is present.
	//
	// RFC 7162 §3.1.3.1 gives an absent modifier and an explicit
	// "UNCHANGEDSINCE 0" opposite meanings: absent means an unconditional
	// store, while "UNCHANGEDSINCE 0" is the always-fail probe — the store must
	// fail for every message that has a modification sequence. UnchangedSince
	// alone cannot tell the two apart, so presence is carried here.
	//
	// The zero value preserves the historical meaning of a zero UnchangedSince
	// ("modifier absent"), so code that only sets UnchangedSince keeps working.
	// Consumers should use Conditional instead of inspecting the two fields by
	// hand.
	UnchangedSinceSet bool

	// UIDStore reports whether the command was issued as UID STORE rather than
	// STORE.
	//
	// It is set by imapserver when invoking Session.Store, where it is the
	// only way to tell the two commands apart when the NumSet is the SEARCHRES
	// marker "$" (which decodes as a UIDSet regardless of the command's number
	// space). Backends that report MODIFIED (RFC 7162 §3.1.3) need it to pick
	// the right number space for the response code.
	//
	// Client.Store ignores this field: the client derives the command kind
	// from the NumSet it is given.
	UIDStore bool
}

// Conditional reports whether the STORE is conditional, i.e. whether the
// UNCHANGEDSINCE modifier (RFC 7162 §3.1.3.1) is present.
//
// It is the single source of truth for the presence rule: the modifier is
// present when either UnchangedSinceSet is true or UnchangedSince is non-zero
// (the latter for callers written before UnchangedSinceSet existed). A nil
// receiver means no options, i.e. an unconditional store.
func (o *StoreOptions) Conditional() bool {
	return o != nil && (o.UnchangedSinceSet || o.UnchangedSince != 0)
}

// StoreFlagsOp is a flag operation: set, add or delete.
type StoreFlagsOp int

const (
	StoreFlagsSet StoreFlagsOp = iota
	StoreFlagsAdd
	StoreFlagsDel
)

// StoreFlags alters message flags.
type StoreFlags struct {
	Op     StoreFlagsOp
	Silent bool
	Flags  []Flag
}
