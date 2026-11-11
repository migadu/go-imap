package imap

import (
	"time"
)

// AppendOptions contains options for the APPEND command.
type AppendOptions struct {
	Flags []Flag
	Time  time.Time

	// Binary sends the message as a literal8, which may contain NUL octets
	// (RFC 3516 section 4.4). Requires BINARY. The server sets it for the
	// session when the client sent the message that way, so a backend which
	// cannot store binary content can answer NO [UNKNOWN-CTE].
	Binary bool
}

// AppendData is the data returned by an APPEND command.
type AppendData struct {
	// requires UIDPLUS or IMAP4rev2
	UID         UID
	UIDValidity uint32
}
