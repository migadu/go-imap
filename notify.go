package imap

// NotifyEvent represents an event type for the NOTIFY command (RFC 5465).
type NotifyEvent string

const (
	// Message events
	NotifyEventFlagChange       NotifyEvent = "FlagChange"
	NotifyEventAnnotationChange NotifyEvent = "AnnotationChange"
	NotifyEventMessageNew       NotifyEvent = "MessageNew"
	NotifyEventMessageExpunge   NotifyEvent = "MessageExpunge"

	// Mailbox events
	NotifyEventMailboxName           NotifyEvent = "MailboxName"
	NotifyEventSubscriptionChange    NotifyEvent = "SubscriptionChange"
	NotifyEventMailboxMetadataChange NotifyEvent = "MailboxMetadataChange"
	NotifyEventServerMetadataChange  NotifyEvent = "ServerMetadataChange"
)

// IsMessageEvent reports whether the event is a <message-event> as defined by
// RFC 5465 section 8: MessageNew, MessageExpunge, FlagChange or
// AnnotationChange. Only message events may be used with the SELECTED and
// SELECTED-DELAYED mailbox specifiers (RFC 5465 section 6.1).
func (event NotifyEvent) IsMessageEvent() bool {
	switch event {
	case NotifyEventMessageNew, NotifyEventMessageExpunge, NotifyEventFlagChange, NotifyEventAnnotationChange:
		return true
	default:
		return false
	}
}

// NotifyMailboxSpec represents a mailbox specifier RFC 5465 section 6 for the NOTIFY command.
type NotifyMailboxSpec string

const (
	NotifyMailboxSpecSelected        NotifyMailboxSpec = "SELECTED"
	NotifyMailboxSpecSelectedDelayed NotifyMailboxSpec = "SELECTED-DELAYED"
	NotifyMailboxSpecPersonal        NotifyMailboxSpec = "PERSONAL"
	NotifyMailboxSpecInboxes         NotifyMailboxSpec = "INBOXES"
	NotifyMailboxSpecSubscribed      NotifyMailboxSpec = "SUBSCRIBED"
)

// NotifyOptions contains options for the NOTIFY command.
type NotifyOptions struct {
	// Status indicates that a STATUS response should be sent for each
	// non-selected mailbox with message events enabled, before NOTIFY's
	// tagged OK (RFC 5465 section 3.1).
	Status bool

	// Items represents the mailbox and events to monitor.
	Items []NotifyItem
}

// NotifyItem represents a mailbox or mailbox set and its events.
type NotifyItem struct {
	// MailboxSpec is a special mailbox specifier (Selected, Personal, etc.)
	// If empty, Mailboxes must be non-empty.
	MailboxSpec NotifyMailboxSpec

	// Mailboxes is a list of specific mailboxes to monitor.
	//
	// Note that RFC 5465 section 6.6 forbids wildcard expansion: '*' and '%'
	// have no special meaning in these names.
	Mailboxes []string

	// Subtree indicates that all mailboxes under the specified mailboxes
	// should be monitored (recursive).
	Subtree bool

	// Events is the list of events to monitor for these mailboxes.
	//
	// An empty list corresponds to the NONE event specifier (RFC 5465
	// section 5): no events are requested for these mailboxes.
	Events []NotifyEvent

	// MessageNewFetch contains the optional fetch attributes of the
	// MessageNew event (RFC 5465 section 5.2). When a new message appears in
	// the selected mailbox, the server sends a FETCH response with these
	// items following the EXISTS response.
	//
	// It is only valid when Events contains NotifyEventMessageNew and
	// MailboxSpec is SELECTED or SELECTED-DELAYED.
	MessageNewFetch *FetchOptions
}
