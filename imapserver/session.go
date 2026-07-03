package imapserver

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
	"github.com/emersion/go-sasl"
)

var errAuthFailed = &imap.Error{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeAuthenticationFailed,
	Text: "Authentication failed",
}

// ErrAuthFailed is returned by Session.Login on authentication failure.
var ErrAuthFailed = errAuthFailed

// GreetingData is the data associated with an IMAP greeting.
type GreetingData struct {
	PreAuth bool
}

// NumKind describes how a number should be interpreted: either as a sequence
// number, either as a UID.
type NumKind int

const (
	NumKindSeq = NumKind(imapwire.NumKindSeq)
	NumKindUID = NumKind(imapwire.NumKindUID)
)

// String implements fmt.Stringer.
func (kind NumKind) String() string {
	switch kind {
	case NumKindSeq:
		return "seq"
	case NumKindUID:
		return "uid"
	default:
		panic(fmt.Errorf("imapserver: unknown NumKind %d", kind))
	}
}

func (kind NumKind) wire() imapwire.NumKind {
	return imapwire.NumKind(kind)
}

// Session is an IMAP session.
//
// The context passed to blocking methods is cancelled when the connection is
// torn down (client disconnect or server shutdown). Backends should propagate
// it into blocking work (database queries, object-storage reads, …) so that
// in-flight operations are abandoned when the client goes away. It is the same
// context returned by Conn.Context.
type Session interface {
	Close() error

	// Not authenticated state
	Login(ctx context.Context, username, password string) error

	// Authenticated state
	Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error)
	Create(ctx context.Context, mailbox string, options *imap.CreateOptions) error
	Delete(ctx context.Context, mailbox string) error
	Rename(ctx context.Context, w *RenameWriter, mailbox, newName string, options *imap.RenameOptions) error
	Subscribe(ctx context.Context, mailbox string) error
	Unsubscribe(ctx context.Context, mailbox string) error
	List(ctx context.Context, w *ListWriter, ref string, patterns []string, options *imap.ListOptions) error
	Status(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error)
	Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error)
	Poll(ctx context.Context, w *UpdateWriter, allowExpunge bool) error
	Idle(ctx context.Context, w *UpdateWriter, stop <-chan struct{}) error

	// Selected state
	Unselect(ctx context.Context) error
	Expunge(ctx context.Context, w *ExpungeWriter, uids *imap.UIDSet) error
	Search(ctx context.Context, kind NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error)
	Sort(ctx context.Context, kind NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error)
	Fetch(ctx context.Context, w *FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error
	Store(ctx context.Context, w *FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error
	Copy(ctx context.Context, numSet imap.NumSet, dest string) (*imap.CopyData, error)
}

// SessionNamespace is an IMAP session which supports NAMESPACE.
type SessionNamespace interface {
	Session

	// Authenticated state
	Namespace(ctx context.Context) (*imap.NamespaceData, error)
}

// SessionMove is an IMAP session which supports MOVE.
type SessionMove interface {
	Session

	// Selected state
	Move(ctx context.Context, w *MoveWriter, numSet imap.NumSet, dest string) error
}

// SessionIMAP4rev2 is an IMAP session which supports IMAP4rev2.
type SessionIMAP4rev2 interface {
	Session
	SessionNamespace
	SessionMove
}

// SessionSASL is an IMAP session which supports its own set of SASL
// authentication mechanisms.
type SessionSASL interface {
	Session
	AuthenticateMechanisms() []string
	Authenticate(mech string) (sasl.Server, error)
}

// SessionUnauthenticate is an IMAP session which supports UNAUTHENTICATE.
type SessionUnauthenticate interface {
	Session

	// Authenticated state
	Unauthenticate(ctx context.Context) error
}

// SessionAppendLimit is an IMAP session which has the same APPEND limit for
// all mailboxes.
type SessionAppendLimit interface {
	Session

	// AppendLimit returns the maximum size in bytes that can be uploaded to
	// this server in an APPEND command.
	AppendLimit() uint32
}

// SessionCapabilities is an IMAP session which can provide its current
// capabilities for capability filtering.
type SessionCapabilities interface {
	Session

	// GetCapabilities returns the session-specific capabilities.
	// This allows sessions to filter capabilities based on client behavior
	// or other session-specific factors.
	GetCapabilities() imap.CapSet
}

// SessionAdditionalCaps is an IMAP session which advertises extra, non-standard
// capability tokens verbatim, in addition to the capabilities the server derives
// from the session's feature set.
//
// Unlike GetCapabilities (which is filtered through the server's known-capability
// allowlist in availableCaps), the tokens returned here are emitted as-is and in
// every connection state — including the unauthenticated greeting. This is the
// mechanism for advertising vendor/experimental tokens that the library does not
// otherwise recognize (e.g. a server-type fingerprint a client keys behavior on).
type SessionAdditionalCaps interface {
	Session

	// AdditionalCapabilities returns extra capability tokens to advertise
	// verbatim. They are appended (de-duplicated) to the standard set.
	AdditionalCapabilities() []imap.Cap
}

// SessionMetadata is an IMAP session which supports the METADATA extension (RFC 5464).
type SessionMetadata interface {
	Session

	// GetMetadata retrieves server or mailbox annotations.
	// If mailbox is empty string "", retrieve server annotations.
	// entries contains the list of entry names to retrieve.
	GetMetadata(ctx context.Context, mailbox string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error)

	// SetMetadata sets or removes server or mailbox annotations.
	// If mailbox is empty string "", set server annotations.
	// To remove an entry, set its value to nil.
	SetMetadata(ctx context.Context, mailbox string, entries map[string]*[]byte) error
}
