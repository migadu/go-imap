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

	// Poll delivers pending updates for the selected mailbox at a command sync
	// point. Pending updates must stay queued when they cannot be delivered, so
	// that the client's sequence-number view stays consistent.
	//
	// With the NOTIFY extension (RFC 5465) in use, the library does not call
	// Poll at all when the client's watch disables message events for the
	// selected mailbox; see SessionNotify for the events the backend itself
	// must filter.
	Poll(ctx context.Context, w *UpdateWriter, allowExpunge bool) error

	// Idle continuously delivers updates until stop is closed.
	//
	// A backend implementing SessionNotify must honor the installed watch here
	// as well: RFC 5465 section 4 requires IDLE to deliver exactly the events
	// the client requested with NOTIFY. The usual implementation lets the
	// NotifyPoll pump be the only source of updates while a watch is installed,
	// and simply waits for stop.
	Idle(ctx context.Context, w *UpdateWriter, stop <-chan struct{}) error

	// Selected state
	Unselect(ctx context.Context) error
	Expunge(ctx context.Context, w *ExpungeWriter, uids *imap.UIDSet) error
	Search(ctx context.Context, kind NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error)
	Sort(ctx context.Context, kind NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error)
	Fetch(ctx context.Context, w *FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error
	// Store alters message flags. A conditional store (options.Conditional)
	// that leaves messages untouched because they failed the UNCHANGEDSINCE
	// precondition reports them by returning an *imap.Error whose Type is
	// StatusResponseTypeOK (or StatusResponseTypeNo for expunged messages) and
	// whose Code carries the failed set — see imap.ModifiedResponseCode. Such
	// an error is a successful completion, not a failure: wrappers must
	// forward it unmodified (or wrap with %w), or the tagged OK [MODIFIED]
	// line becomes an internal server error.
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

// SessionNotify is an IMAP session which supports the NOTIFY extension
// (RFC 5465).
//
// The connection advertises the NOTIFY capability only when the session
// implements this interface (and the server is configured with
// imap.CapNotify).
type SessionNotify interface {
	Session

	// SetNotify installs, replaces or clears the connection's notification
	// watch. It is invoked synchronously on the connection's command
	// goroutine while no NotifyPoll call is running.
	//
	// options is nil for NOTIFY NONE, otherwise it describes the requested
	// watch. The options have already been checked against the structural
	// rules of RFC 5465 (event pairing, single SELECTED group, fetch-atts
	// placement); the backend is responsible for rejecting events it does
	// not support by returning *UnsupportedNotifyEventError, which the
	// server translates into a tagged NO with the BADEVENT response code.
	//
	// The per-mailbox access checks of RFC 5465 section 3.1 are also the
	// backend's: a named mailbox that does not exist, or that the user cannot
	// LIST, must be ignored silently, and one the user can LIST but lacks the
	// rights to monitor must be reported with an untagged LIST carrying
	// imap.MailboxAttrNoAccess (written with UpdateWriter.WriteList). Section
	// 5.9 extends this to later changes: monitoring stops when access is lost
	// and resumes when it is granted again.
	//
	// If options.Status is set, the implementation must write the initial
	// STATUS responses for matching non-selected mailboxes to w before
	// returning, so that they precede NOTIFY's tagged OK (RFC 5465
	// section 3.1).
	//
	// On error, the previously installed watch (if any) must be left in
	// effect.
	SetNotify(ctx context.Context, options *imap.NotifyOptions, w *UpdateWriter) error

	// NotifyPoll delivers notifications for the watch installed by
	// SetNotify, writing to w until stop is closed or ctx is cancelled. It
	// runs on a dedicated goroutine, concurrently with command processing on
	// the connection; individual responses are serialized with command
	// output by the library.
	//
	// Only the message events requested with the SELECTED/SELECTED-DELAYED
	// specifier may be reported for the selected mailbox; omitting that
	// specifier "is the same as specifying SELECTED NONE" (RFC 5465 section
	// 3.1). The library enforces this for the per-command sync points (it
	// skips Session.Poll, and drops unrequested flag updates), but the
	// backend must not push such updates from NotifyPoll or Idle either.
	//
	// EXPUNGE/VANISHED updates for the selected mailbox must only be
	// delivered while w.ExpungeAllowed() reports true (and, with
	// SELECTED-DELAYED, only at the sync points of RFC 5465 section 6.1.2 —
	// most backends simply leave delayed expunges queued for the regular
	// per-command Poll).
	//
	// If no watch is currently installed (e.g. it was dropped after a
	// notification overflow), NotifyPoll returns nil promptly.
	NotifyPoll(ctx context.Context, w *UpdateWriter, stop <-chan struct{}) error
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
