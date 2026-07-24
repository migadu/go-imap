package imapserver

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// UnsupportedNotifyEventError is returned by SessionNotify.SetNotify when the
// client requested an event the backend does not support. The server then
// replies with a tagged NO carrying the BADEVENT response code, which lists
// all events the backend supports (RFC 5465 section 4).
type UnsupportedNotifyEventError struct {
	// Supported is the full list of events supported by the backend,
	// advertised to the client in the BADEVENT response code.
	Supported []imap.NotifyEvent
}

// Error implements the error interface.
func (err *UnsupportedNotifyEventError) Error() string {
	return "imapserver: unsupported NOTIFY event"
}

var notifyEventNames = map[string]imap.NotifyEvent{
	"MESSAGENEW":            imap.NotifyEventMessageNew,
	"MESSAGEEXPUNGE":        imap.NotifyEventMessageExpunge,
	"FLAGCHANGE":            imap.NotifyEventFlagChange,
	"ANNOTATIONCHANGE":      imap.NotifyEventAnnotationChange,
	"MAILBOXNAME":           imap.NotifyEventMailboxName,
	"SUBSCRIPTIONCHANGE":    imap.NotifyEventSubscriptionChange,
	"MAILBOXMETADATACHANGE": imap.NotifyEventMailboxMetadataChange,
	"SERVERMETADATACHANGE":  imap.NotifyEventServerMetadataChange,
}

// canonicalNotifyEvent maps an event atom to its canonical NotifyEvent value.
// Event names are case-insensitive on the wire (RFC 5465 section 8). Unknown
// atoms (user-defined events, event-ext) are preserved verbatim so the
// backend can reject them with UnsupportedNotifyEventError.
func canonicalNotifyEvent(name string) imap.NotifyEvent {
	if ev, ok := notifyEventNames[strings.ToUpper(name)]; ok {
		return ev
	}
	return imap.NotifyEvent(name)
}

func (c *Conn) handleNotify(tag string, dec *imapwire.Decoder) error {
	var verb string
	if !dec.ExpectSP() || !dec.ExpectAtom(&verb) {
		return dec.Err()
	}

	var options *imap.NotifyOptions
	switch strings.ToUpper(verb) {
	case "NONE":
		// options stays nil: disable all notifications
	case "SET":
		options = &imap.NotifyOptions{}
		for dec.SP() {
			if dec.Special('(') {
				item, err := c.readNotifyEventGroup(dec)
				if err != nil {
					return err
				}
				options.Items = append(options.Items, *item)
				continue
			}
			// status-indicator = SP "STATUS": a bare atom before the first
			// event group (RFC 5465 section 8).
			var atom string
			if !dec.ExpectAtom(&atom) {
				return dec.Err()
			}
			if strings.EqualFold(atom, "STATUS") && !options.Status && len(options.Items) == 0 {
				options.Status = true
				continue
			}
			return newClientBugError("Syntax error in NOTIFY command")
		}
		if len(options.Items) == 0 {
			return newClientBugError("NOTIFY SET requires at least one event group")
		}
	default:
		return newClientBugError("NOTIFY argument must be SET or NONE")
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	// Check if NOTIFY is supported by the session (mirrors handleIdle, so a
	// per-session capability filter also gates NOTIFY).
	var supportsNotify bool
	if capSession, ok := c.session.(SessionCapabilities); ok {
		supportsNotify = capSession.GetCapabilities().Has(imap.CapNotify)
	} else {
		supportsNotify = c.availableCapsSet().Has(imap.CapNotify)
	}
	session, ok := c.session.(SessionNotify)
	if !supportsNotify || !ok {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "NOTIFY not supported",
		}
	}

	if options != nil {
		if err := validateNotifyOptions(options); err != nil {
			return err
		}
	}

	// Stop the current pump before touching the backend watch: NotifyPoll
	// must not run while SetNotify swaps the state underneath it.
	hadPump := c.notifyPumpRunning()
	c.stopNotifyPump()

	w := &UpdateWriter{conn: c, allowExpunge: true, notify: true}
	if err := session.SetNotify(c.ctx, options, w); err != nil {
		// RFC 5465 section 3.1: the effect of a successful NOTIFY command
		// lasts until the next NOTIFY command. A failed one leaves the
		// previous watch in effect, so restart its pump.
		if hadPump {
			c.startNotifyPump(session)
		}
		var badEventErr *UnsupportedNotifyEventError
		if errors.As(err, &badEventErr) {
			return c.writeBadEvent(tag, badEventErr.Supported)
		}
		return err
	}

	if options != nil {
		c.startNotifyPump(session)
	}

	// A successful NOTIFY SET implies an implicit NOOP (RFC 5465 section
	// 3.1): flush pending updates for the selected mailbox before the tagged
	// OK. NOTIFY NONE gets the same treatment (it is not one of the commands
	// that forbid expunges).
	if err := c.poll("NOTIFY"); err != nil {
		return err
	}
	return c.writeStatusResp(tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "NOTIFY completed",
	})
}

// readNotifyEventGroup parses one event-group; the opening parenthesis has
// already been consumed:
//
//	event-group = "(" filter-mailboxes SP events ")"
func (c *Conn) readNotifyEventGroup(dec *imapwire.Decoder) (*imap.NotifyItem, error) {
	var filter string
	if !dec.ExpectAtom(&filter) {
		return nil, dec.Err()
	}

	var item imap.NotifyItem
	switch strings.ToUpper(filter) {
	case "SELECTED":
		item.MailboxSpec = imap.NotifyMailboxSpecSelected
	case "SELECTED-DELAYED":
		item.MailboxSpec = imap.NotifyMailboxSpecSelectedDelayed
	case "PERSONAL":
		item.MailboxSpec = imap.NotifyMailboxSpecPersonal
	case "INBOXES":
		item.MailboxSpec = imap.NotifyMailboxSpecInboxes
	case "SUBSCRIBED":
		item.MailboxSpec = imap.NotifyMailboxSpecSubscribed
	case "SUBTREE", "MAILBOXES":
		item.Subtree = strings.EqualFold(filter, "SUBTREE")
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
		// one-or-more-mailbox = mailbox / "(" mailbox *(SP mailbox) ")"
		isList, err := dec.List(func() error {
			var name string
			if !dec.ExpectMailbox(&name) {
				return dec.Err()
			}
			item.Mailboxes = append(item.Mailboxes, name)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !isList {
			var name string
			if !dec.ExpectMailbox(&name) {
				return nil, dec.Err()
			}
			item.Mailboxes = []string{name}
		}
		if len(item.Mailboxes) == 0 {
			return nil, newClientBugError("NOTIFY: at least one mailbox is required")
		}
	default:
		return nil, newClientBugError("NOTIFY: unknown mailbox specifier")
	}

	// events = ("(" event *(SP event) ")") / "NONE"
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	if dec.Special('(') {
		for {
			var name string
			if !dec.ExpectAtom(&name) {
				return nil, dec.Err()
			}
			event := canonicalNotifyEvent(name)
			item.Events = append(item.Events, event)
			if !dec.SP() {
				break
			}
			if dec.Special('(') {
				// message-event = "MessageNew" [SP "(" fetch-att *(SP fetch-att) ")"]
				if event != imap.NotifyEventMessageNew {
					return nil, newClientBugError("NOTIFY: fetch attributes are only allowed after MessageNew")
				}
				if item.MessageNewFetch != nil {
					return nil, newClientBugError("NOTIFY: duplicate MessageNew fetch attributes")
				}
				fetchOptions, err := readNotifyFetchAtts(c, dec)
				if err != nil {
					return nil, err
				}
				item.MessageNewFetch = fetchOptions
				if !dec.SP() {
					break
				}
			}
		}
		if !dec.ExpectSpecial(')') {
			return nil, dec.Err()
		}
	} else {
		var atom string
		if !dec.ExpectAtom(&atom) {
			return nil, dec.Err()
		}
		if !strings.EqualFold(atom, "NONE") {
			return nil, newClientBugError("NOTIFY: expected event list or NONE")
		}
	}

	if !dec.ExpectSpecial(')') {
		return nil, dec.Err()
	}
	return &item, nil
}

// readNotifyFetchAtts parses the fetch-att list of a MessageNew event; the
// opening parenthesis has already been consumed.
func readNotifyFetchAtts(c *Conn, dec *imapwire.Decoder) (*imap.FetchOptions, error) {
	options := &imap.FetchOptions{}
	writerOptions := fetchWriterOptions{obsolete: make(map[*imap.FetchItemBodySection]string)}
	for {
		attName, err := readFetchAttName(dec)
		if err != nil {
			return nil, err
		}
		switch attName {
		case "ALL", "FAST", "FULL":
			return nil, newClientBugError("NOTIFY: FETCH macros are not allowed in MessageNew fetch attributes")
		}
		if err := handleFetchAtt(c, dec, attName, options, &writerOptions); err != nil {
			var imapErr *imap.Error
			if errors.As(err, &imapErr) {
				return nil, err
			}
			return nil, newClientBugError(fmt.Sprintf("NOTIFY: %v", err))
		}
		if !dec.SP() {
			break
		}
	}
	if !dec.ExpectSpecial(')') {
		return nil, dec.Err()
	}
	return options, nil
}

// validateNotifyOptions enforces the structural rules of RFC 5465 sections 5
// and 6.1. Violations yield a tagged BAD response, as required by the RFC.
// Whether each requested event is supported is for the backend to decide (see
// UnsupportedNotifyEventError).
func validateNotifyOptions(options *imap.NotifyOptions) error {
	numSelected := 0
	for i := range options.Items {
		item := &options.Items[i]
		selected := item.MailboxSpec == imap.NotifyMailboxSpecSelected ||
			item.MailboxSpec == imap.NotifyMailboxSpecSelectedDelayed
		if selected {
			numSelected++
			if numSelected > 1 {
				// RFC 5465 section 6.1: only one of the mailbox specifiers
				// affecting the currently selected mailbox can be specified.
				return newClientBugError("NOTIFY: only one SELECTED or SELECTED-DELAYED event group is allowed")
			}
		}

		var hasNew, hasExpunge, hasFlag, hasAnnotation, hasMailboxEvent bool
		for _, ev := range item.Events {
			switch ev {
			case imap.NotifyEventMessageNew:
				hasNew = true
			case imap.NotifyEventMessageExpunge:
				hasExpunge = true
			case imap.NotifyEventFlagChange:
				hasFlag = true
			case imap.NotifyEventAnnotationChange:
				hasAnnotation = true
			case imap.NotifyEventMailboxName, imap.NotifyEventSubscriptionChange,
				imap.NotifyEventMailboxMetadataChange, imap.NotifyEventServerMetadataChange:
				hasMailboxEvent = true
			}
		}

		// RFC 5465 section 5: if one of MessageNew or MessageExpunge is
		// specified, both events MUST be specified.
		if hasNew != hasExpunge {
			return newClientBugError("NOTIFY: MessageNew and MessageExpunge must be specified together")
		}
		// RFC 5465 section 5: FlagChange and/or AnnotationChange require
		// MessageNew and MessageExpunge.
		if (hasFlag || hasAnnotation) && !hasNew {
			return newClientBugError("NOTIFY: FlagChange and AnnotationChange require MessageNew and MessageExpunge")
		}
		// RFC 5465 section 6.1: the SELECTED/SELECTED-DELAYED specifiers only
		// apply to message events.
		if selected && hasMailboxEvent {
			return newClientBugError("NOTIFY: only message events are allowed with SELECTED or SELECTED-DELAYED")
		}
		// RFC 5465 section 5.2: fetch attributes describe the FETCH response
		// sent for new messages in the selected mailbox.
		if !selected && item.MessageNewFetch != nil {
			return newClientBugError("NOTIFY: MessageNew fetch attributes are only allowed with SELECTED or SELECTED-DELAYED")
		}
	}
	return nil
}

// writeBadEvent writes a tagged NO response with the BADEVENT response code,
// listing the events supported by the backend (RFC 5465 section 4).
func (c *Conn) writeBadEvent(tag string, supported []imap.NotifyEvent) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	if tag == "" {
		tag = "*"
	}
	enc.Atom(tag).SP().Atom(string(imap.StatusResponseTypeNo)).SP()
	// unsupported-events-code = "BADEVENT" SP "(" event-name *(SP event-name) ")"
	if len(supported) > 0 {
		enc.Special('[').Atom(string(imap.ResponseCodeBadEvent)).SP()
		enc.List(len(supported), func(i int) {
			enc.Atom(string(supported[i]))
		})
		enc.Special(']').SP()
	}
	enc.Text("Unsupported NOTIFY event")
	return enc.CRLF()
}

func (c *Conn) notifyPumpRunning() bool {
	c.notifyMutex.Lock()
	defer c.notifyMutex.Unlock()
	return c.notifyStop != nil
}

// startNotifyPump spawns the goroutine that runs SessionNotify.NotifyPoll,
// delivering unsolicited notifications concurrently with command processing.
func (c *Conn) startNotifyPump(session SessionNotify) {
	stop := make(chan struct{})
	done := make(chan error, 1)

	c.notifyMutex.Lock()
	c.notifyStop = stop
	c.notifyDone = done
	c.notifyMutex.Unlock()

	w := &UpdateWriter{conn: c, allowExpunge: true, notify: true}
	go func() {
		defer func() {
			if v := recover(); v != nil {
				c.server.logger().Printf("panic in NOTIFY: %v\n%s", v, debug.Stack())
				done <- fmt.Errorf("imapserver: panic in NOTIFY")
			}
		}()
		done <- session.NotifyPoll(c.ctx, w, stop)
	}()
}

// stopNotifyPump stops the notify pump, if one is running, and waits for the
// backend to return (with a timeout guard against goroutine leaks, mirroring
// handleIdle).
func (c *Conn) stopNotifyPump() {
	c.notifyMutex.Lock()
	stop, done := c.notifyStop, c.notifyDone
	c.notifyStop, c.notifyDone = nil, nil
	c.notifyMutex.Unlock()

	if stop == nil {
		return
	}
	close(stop)

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil && !isConnectionClosedError(err) {
			c.server.logger().Printf("NOTIFY backend error: %v", err)
		}
	case <-timer.C:
		c.server.logger().Printf("NOTIFY backend did not return within 30s after stop; goroutine leaked")
	}
}
