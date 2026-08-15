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

const (
	// maxNotifyEventGroups bounds the number of event-groups in one NOTIFY SET
	// command. RFC 5465 §3.1 advises clients to limit the mailboxes they watch;
	// the server enforces a ceiling so a single command cannot hand the backend
	// an arbitrarily large watch list (bounded otherwise only by the 50 KiB
	// command-size cap).
	maxNotifyEventGroups = 64

	// maxNotifyMailboxesPerGroup bounds the mailbox names in one event-group's
	// mailbox list. With maxNotifyEventGroups this bounds the total watch size.
	maxNotifyMailboxesPerGroup = 256
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
	// pendingFetchWriterOptions carries the MessageNew fetch-att writer options
	// from the parser to the point where the new watch is installed.
	var pendingFetchWriterOptions *fetchWriterOptions

	var verb string
	if !dec.ExpectSP() || !dec.ExpectAtom(&verb) {
		return dec.Err()
	}

	var (
		options  *imap.NotifyOptions
		overflow bool // a size cap was exceeded; refuse once the command is fully parsed
	)
	switch strings.ToUpper(verb) {
	case "NONE":
		// options stays nil: disable all notifications
	case "SET":
		options = &imap.NotifyOptions{}
		for dec.SP() {
			if dec.Special('(') {
				// Parse every group, even past the cap: the command is
				// syntactically valid (RFC 5465 §8 bounds neither event-groups
				// nor many-mailboxes), so it must be consumed in full — a
				// literal left unread would be parsed as the next command.
				// Groups beyond the cap are discarded rather than stored, so
				// the watch stays bounded, and the refusal is sent once the
				// whole command has been read.
				item, groupOverflow, err := c.readNotifyEventGroup(dec, &pendingFetchWriterOptions)
				if err != nil {
					return err
				}
				if groupOverflow || len(options.Items) >= maxNotifyEventGroups {
					overflow = true
					continue
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
		if overflow {
			// RFC 5465 §3.1: the server MAY refuse a watch that would be
			// prohibitively expensive with NO [NOTIFICATIONOVERFLOW].
			if !dec.ExpectCRLF() {
				return dec.Err()
			}
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeNotificationOverflow,
				Text: "NOTIFY watch too large",
			}
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

	// Record which message events the client wants for the selected mailbox, so
	// that the per-command sync points stop reporting the ones it did not ask
	// for (RFC 5465 section 3.1), together with the response-writer options of
	// the MessageNew fetch-atts.
	c.setNotifySelectedEvents(options, pendingFetchWriterOptions)

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
//
// The returned overflow flag reports that the group named more mailboxes than
// maxNotifyMailboxesPerGroup. The group is still parsed to its end so the
// command stream stays in sync; the caller turns the flag into a refusal.
func (c *Conn) readNotifyEventGroup(dec *imapwire.Decoder, pendingFetchWriterOptions **fetchWriterOptions) (item *imap.NotifyItem, overflow bool, err error) {
	var filter string
	if !dec.ExpectAtom(&filter) {
		return nil, false, dec.Err()
	}

	var group imap.NotifyItem
	switch strings.ToUpper(filter) {
	case "SELECTED":
		group.MailboxSpec = imap.NotifyMailboxSpecSelected
	case "SELECTED-DELAYED":
		group.MailboxSpec = imap.NotifyMailboxSpecSelectedDelayed
	case "PERSONAL":
		group.MailboxSpec = imap.NotifyMailboxSpecPersonal
	case "INBOXES":
		group.MailboxSpec = imap.NotifyMailboxSpecInboxes
	case "SUBSCRIBED":
		group.MailboxSpec = imap.NotifyMailboxSpecSubscribed
	case "SUBTREE", "MAILBOXES":
		group.Subtree = strings.EqualFold(filter, "SUBTREE")
		if !dec.ExpectSP() {
			return nil, false, dec.Err()
		}
		// one-or-more-mailbox = mailbox / "(" mailbox *(SP mailbox) ")"
		isList, err := dec.List(func() error {
			var name string
			if !dec.ExpectMailbox(&name) {
				return dec.Err()
			}
			if len(group.Mailboxes) >= maxNotifyMailboxesPerGroup {
				// Keep consuming the list, but stop growing the watch.
				overflow = true
				return nil
			}
			group.Mailboxes = append(group.Mailboxes, name)
			return nil
		})
		if err != nil {
			return nil, false, err
		}
		if !isList {
			var name string
			if !dec.ExpectMailbox(&name) {
				return nil, false, dec.Err()
			}
			group.Mailboxes = []string{name}
		}
		if len(group.Mailboxes) == 0 {
			return nil, false, newClientBugError("NOTIFY: at least one mailbox is required")
		}
	default:
		return nil, false, newClientBugError("NOTIFY: unknown mailbox specifier")
	}

	// events = ("(" event *(SP event) ")") / "NONE"
	if !dec.ExpectSP() {
		return nil, false, dec.Err()
	}
	if dec.Special('(') {
		for {
			var name string
			if !dec.ExpectAtom(&name) {
				return nil, false, dec.Err()
			}
			event := canonicalNotifyEvent(name)
			group.Events = append(group.Events, event)
			if !dec.SP() {
				break
			}
			if dec.Special('(') {
				// message-event = "MessageNew" [SP "(" fetch-att *(SP fetch-att) ")"]
				if event != imap.NotifyEventMessageNew {
					return nil, false, newClientBugError("NOTIFY: fetch attributes are only allowed after MessageNew")
				}
				if group.MessageNewFetch != nil {
					return nil, false, newClientBugError("NOTIFY: duplicate MessageNew fetch attributes")
				}
				fetchOptions, fetchWriterOpts, err := readNotifyFetchAtts(c, dec)
				if err != nil {
					return nil, false, err
				}
				group.MessageNewFetch = fetchOptions
				// Remember how the client spelled the fetch-atts so the
				// notification's FETCH response uses the same item names
				// (RFC 5465 §5.2: "the information requested by the
				// client"). Applied once the watch is installed.
				*pendingFetchWriterOptions = fetchWriterOpts
				if !dec.SP() {
					break
				}
			}
		}
		if !dec.ExpectSpecial(')') {
			return nil, false, dec.Err()
		}
	} else {
		var atom string
		if !dec.ExpectAtom(&atom) {
			return nil, false, dec.Err()
		}
		if !strings.EqualFold(atom, "NONE") {
			return nil, false, newClientBugError("NOTIFY: expected event list or NONE")
		}
	}

	if !dec.ExpectSpecial(')') {
		return nil, false, dec.Err()
	}
	return &group, overflow, nil
}

// readNotifyFetchAtts parses the fetch-att list of a MessageNew event; the
// opening parenthesis has already been consumed.
func readNotifyFetchAtts(c *Conn, dec *imapwire.Decoder) (*imap.FetchOptions, *fetchWriterOptions, error) {
	options := &imap.FetchOptions{}
	writerOptions := &fetchWriterOptions{obsolete: make(map[*imap.FetchItemBodySection]string)}
	for {
		attName, err := readFetchAttName(dec)
		if err != nil {
			return nil, nil, err
		}
		switch attName {
		case "ALL", "FAST", "FULL":
			return nil, nil, newClientBugError("NOTIFY: FETCH macros are not allowed in MessageNew fetch attributes")
		}
		if err := handleFetchAtt(c, dec, attName, options, writerOptions); err != nil {
			var imapErr *imap.Error
			if errors.As(err, &imapErr) {
				return nil, nil, err
			}
			return nil, nil, newClientBugError(fmt.Sprintf("NOTIFY: %v", err))
		}
		if !dec.SP() {
			break
		}
	}
	if !dec.ExpectSpecial(')') {
		return nil, nil, dec.Err()
	}
	return options, writerOptions, nil
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

// fenceNotifyPump stops the NOTIFY pump and reports whether one was running,
// so the caller can restart it with restartNotifyPump once it is done. It is
// used around changes of the selected mailbox, which the running watch refers
// to (RFC 5465 section 6.1).
func (c *Conn) fenceNotifyPump() bool {
	running := c.notifyPumpRunning()
	c.stopNotifyPump()
	return running
}

// restartNotifyPump restarts a pump stopped by fenceNotifyPump. It is a no-op
// when no pump was running, when the session no longer supports NOTIFY, or
// when the connection is no longer authenticated.
func (c *Conn) restartNotifyPump(running bool) {
	if !running {
		return
	}
	session, ok := c.session.(SessionNotify)
	if !ok {
		return
	}
	switch c.state {
	case imap.ConnStateAuthenticated, imap.ConnStateSelected:
		c.startNotifyPump(session)
	}
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
		var err error
		defer func() {
			if v := recover(); v != nil {
				c.server.logger().Printf("panic in NOTIFY: %v\n%s", v, debug.Stack())
				err = fmt.Errorf("imapserver: panic in NOTIFY")
			}
			done <- err

			// A pump that ends because stop was closed is an intentional stop
			// (NOTIFY NONE, watch replacement, teardown); stopNotifyPump owns the
			// aftermath. A pump that ends any OTHER way — NotifyPoll returning a
			// non-nil error, or a panic — is a broken watch on a live connection:
			// the client still believes it is being notified, but nothing is
			// delivered. That silent failure is exactly what NOTIFY exists to
			// avoid, so tear the connection down. A clean nil return with stop
			// still open (e.g. NOTIFICATIONOVERFLOW: the backend disabled the
			// watch and returned) is deliberate and leaves the connection up.
			select {
			case <-stop:
			default:
				if err != nil {
					c.abortFromBackground("NOTIFY pump", err)
				}
			}
		}()
		err = session.NotifyPoll(c.ctx, w, stop)
	}()
}

// abortFromBackground tears the connection down from a goroutine other than the
// command loop (the NOTIFY pump). It cancels the context and closes the socket
// directly rather than writing a BYE: the socket close unblocks the command
// read loop without acquiring encMutex, so it works even if a wedged response
// writer is still holding that mutex. serve()'s deferred teardown then runs
// normally. Safe to call concurrently with serve exit — cancel and Close are
// both idempotent.
func (c *Conn) abortFromBackground(who string, err error) {
	if err != nil && !isConnectionClosedError(err) {
		c.server.logger().Printf("%s aborting connection (remote %v): %v", who, c.conn.RemoteAddr(), err)
	}
	c.cancel()
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()
	conn.Close()
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
		// The pump ignored stop (or is wedged in a slow write). We must not
		// return and let the caller start a second pump against session state
		// that assumes a single one. Force the socket closed: that unblocks a
		// wedged write so the goroutine can exit, and cancels the context so a
		// replacement pump (if the caller starts one) sees a dead connection
		// and returns immediately.
		c.server.logger().Printf("NOTIFY backend did not return within 30s after stop; forcing connection close")
		c.abortFromBackground("NOTIFY stop timeout", nil)
	}
}
