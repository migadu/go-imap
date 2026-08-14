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
	// Consumers must therefore treat the store as conditional when either
	// UnchangedSinceSet is true or UnchangedSince is non-zero.
	UnchangedSinceSet bool
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
