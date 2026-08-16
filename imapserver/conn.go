package imapserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

const (
	cmdReadTimeout     = 30 * time.Second
	idleReadTimeout    = 35 * time.Minute // section 5.4 says 30min minimum
	literalReadTimeout = 5 * time.Minute

	respWriteTimeout    = 30 * time.Second
	literalWriteTimeout = 5 * time.Minute

	maxCommandSize = 50 * 1024 // RFC 2683 section 3.2.1.5 says 8KiB minimum
)

var internalServerErrorResp = &imap.StatusResponse{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeServerBug,
	Text: "Internal server error",
}

// isConnectionClosedError returns true for errors that indicate the remote
// end has disconnected. These are normal during client/proxy disconnect and
// should not be logged as errors.
func isConnectionClosedError(err error) bool {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.EPIPE) || errors.Is(opErr.Err, syscall.ECONNRESET) {
			return true
		}
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		if errors.Is(syscallErr.Err, syscall.EPIPE) || errors.Is(syscallErr.Err, syscall.ECONNRESET) {
			return true
		}
	}
	return false
}

// A Conn represents an IMAP connection to the server.
type Conn struct {
	server   *Server
	br       *bufio.Reader
	bw       *bufio.Writer
	encMutex sync.Mutex

	// ctx is cancelled when the connection is torn down (client disconnect,
	// serve goroutine exit, or server shutdown via forceCloseConns). It is the
	// context passed to blocking Session methods so a backend can abandon
	// in-flight work when the connection goes away. cancel is idempotent.
	ctx    context.Context
	cancel context.CancelFunc

	mutex     sync.Mutex
	conn      net.Conn
	enabled   imap.CapSet
	condStore bool         // client issued a CONDSTORE-enabling command (RFC 7162 §3.1)
	clientID  *imap.IDData // Store client identification info

	state   imap.ConnState
	session Session

	// idle is true while the connection has nothing in flight and is blocked
	// waiting for client input: between commands, or inside IDLE waiting for
	// DONE. shuttingDown is set by Server.Shutdown and means "send BYE and exit
	// at the next idle point -- or now, if already idle". Both are guarded by
	// mutex; see setIdle, setActive and requestShutdown for the protocol.
	idle         bool
	shuttingDown bool

	// selectedReadOnly is true when the selected mailbox is read-only, either
	// because it was opened with EXAMINE or because the session reported it
	// read-only (RFC 4314 §5.2). Only touched from the command-loop goroutine,
	// like state.
	selectedReadOnly bool

	// NOTIFY (RFC 5465) pump state. notifyStop/notifyDone are non-nil while a
	// SessionNotify.NotifyPoll goroutine is running for this connection.
	notifyMutex sync.Mutex
	notifyStop  chan struct{}
	notifyDone  chan error

	// notifyUsed reports whether the client has issued a NOTIFY command on this
	// connection. Until it does, the legacy behaviour of RFC 5465 §3.1 applies:
	// message events for the selected mailbox are reported while a command is
	// being processed.
	//
	// notifySelectedEvents is the set of message events requested with the
	// SELECTED/SELECTED-DELAYED specifier. It is empty after NOTIFY NONE, or
	// when NOTIFY SET carried no such event group — which RFC 5465 §3.1 defines
	// as "the same as specifying SELECTED NONE". Both are guarded by
	// notifyMutex, as the pump goroutine reads them.
	notifyUsed           bool
	notifySelectedEvents map[imap.NotifyEvent]bool

	// notifyFetchWriterOptions holds the response-writer options of the
	// MessageNew fetch-att list of the installed watch, so unsolicited FETCH
	// responses use the data-item names the client asked for.
	notifyFetchWriterOptions *fetchWriterOptions

	// activeCmd is the name of the command currently being processed by the
	// command goroutine ("" between commands). The NOTIFY pump consults it to
	// avoid delivering EXPUNGE/VANISHED updates while a command that forbids
	// them (FETCH/STORE/SEARCH, RFC 3501 §5.5) is in progress.
	cmdMutex  sync.Mutex
	activeCmd string
}

func newConn(c net.Conn, server *Server) *Conn {
	rw := server.options.wrapReadWriter(c)
	br := bufio.NewReader(rw)
	bw := bufio.NewWriter(rw)
	// The context is created here, before the connection is registered with the
	// server, so forceCloseConns can always cancel it.
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{
		conn:    c,
		server:  server,
		br:      br,
		bw:      bw,
		ctx:     ctx,
		cancel:  cancel,
		enabled: make(imap.CapSet),
	}
}

// NetConn returns the underlying connection that is wrapped by the IMAP
// connection.
//
// Writing to or reading from this connection directly will corrupt the IMAP
// session.
func (c *Conn) NetConn() net.Conn {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.conn
}

// Bye terminates the IMAP connection.
func (c *Conn) Bye(text string) error {
	respErr := c.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeBye,
		Text: text,
	})
	// Read c.conn under the mutex: STARTTLS reassigns it and forceCloseConns
	// closes it from other goroutines.
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()
	closeErr := conn.Close()
	if respErr != nil {
		return respErr
	}
	return closeErr
}

func (c *Conn) EnabledCaps() imap.CapSet {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.enabled.Copy()
}

// enabledHas reports whether cap has been ENABLEd on this connection. It takes
// c.mutex so it is safe to call from UpdateWriter/tracker callbacks that a
// backend may invoke from its own goroutine, concurrently with ENABLE writing
// c.enabled on the command goroutine (c.enabled is a plain map).
func (c *Conn) enabledHas(cap imap.Cap) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.enabled.Has(cap)
}

// isIMAP4rev2 reports whether this connection should be served IMAP4rev2
// semantics, which is true in two cases and not one:
//
//   - the client sent ENABLE IMAP4rev2, or
//   - the server does not advertise IMAP4rev1 at all, so there are no legacy
//     clients to protect and every session is an IMAP4rev2 session whether or
//     not it bothered to ENABLE anything.
//
// Note it deliberately does NOT read the ADVERTISED IMAP4rev2 capability: a
// dual-stack server (IMAP4rev1 + IMAP4rev2) must keep sending the IMAP4rev1
// wire form to clients that never enabled rev2.
//
// This was open-coded at four call sites in three different spellings before it
// was a function, and a fifth writer got it wrong by checking only the enabled
// half — which left a rev2-only server answering deprecated response forms.
//
// Callers already holding c.mutex must not use this (it locks): see
// useQuotedUTF8, which inlines the same test for that reason.
func (c *Conn) isIMAP4rev2() bool {
	return c.enabledHas(imap.CapIMAP4rev2) ||
		!c.server.options.caps().Has(imap.CapIMAP4rev1)
}

// IsIMAP4rev2 reports whether this connection should be served IMAP4rev2
// semantics (either IMAP4rev2 was enabled, or IMAP4rev1 is not advertised).
func (c *Conn) IsIMAP4rev2() bool {
	return c.isIMAP4rev2()
}

// Context returns the connection's context. It is cancelled when the
// connection is torn down — by client disconnect, by the serve goroutine
// exiting, or by server shutdown. Backends may use it to bound blocking work,
// though blocking Session methods already receive it as their first argument.
func (c *Conn) Context() context.Context {
	return c.ctx
}

// errShutdown is returned by a command handler that was parked waiting for
// client input when Server.Shutdown asked the connection to finish. It carries
// no tagged response: the serve loop answers it with BYE and exits.
var errShutdown = errors.New("imapserver: server shutting down")

// setIdle records that the connection is about to block waiting for client
// input with nothing in flight, and reports whether it may. False means
// Shutdown has already asked this connection to finish, so the caller should
// send BYE now instead of waiting.
//
// setIdle, setActive and requestShutdown together make the idle/active
// decision atomic with the transition. That is what keeps a BYE from ever
// landing in the middle of a response: Shutdown only wakes a connection it has
// seen idle under the mutex, and a connection only starts processing input
// after re-checking under the same mutex that Shutdown has not spoken.
func (c *Conn) setIdle() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.shuttingDown {
		return false
	}
	c.idle = true
	return true
}

// setActive records that the wait for client input is over and reports
// whether the input may be processed. False means Shutdown asked the
// connection to finish while it was waiting; whatever arrived is dropped in
// favour of BYE, exactly as if it had arrived after the connection closed.
func (c *Conn) setActive() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.idle = false
	return !c.shuttingDown
}

// requestShutdown asks the connection to finish gracefully. An idle
// connection is woken so it can send BYE right away; an active one will do so
// once its current command completes.
//
// Shutdown never writes to the connection itself. The serve goroutine owns
// every byte of output including the BYE, so response ordering needs no
// further coordination.
func (c *Conn) requestShutdown() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.shuttingDown = true
	if c.idle {
		// Interrupt the blocked read. The serve goroutine re-arms the deadline
		// before every read it makes after this, so a deadline in the past
		// cannot leak into a later command.
		c.conn.SetReadDeadline(time.Now())
	}
}

func (c *Conn) serve() {
	// Cancel the connection context on any exit so blocking backend work is
	// released even if the socket close alone doesn't unblock it.
	defer c.cancel()
	defer func() {
		if v := recover(); v != nil {
			c.server.logger().Printf("panic handling command (remote %v): %v\n%s", c.conn.RemoteAddr(), v, debug.Stack())
		}

		c.conn.Close()
	}()

	c.server.mutex.Lock()
	if c.server.closed {
		// The server was closed between the accept and this registration.
		// Close sets closed before forceCloseConns snapshots s.conns, both
		// under this mutex, so an entry added now would never be
		// force-closed and the session would outlive Close. Send a
		// best-effort BYE (like rejectConn, off the mutex and bounded by a
		// write deadline) and bail out; the deferred conn.Close and cancel
		// handle cleanup.
		c.server.mutex.Unlock()
		c.conn.SetWriteDeadline(time.Now().Add(respWriteTimeout))
		c.conn.Write([]byte("* BYE Server shutting down\r\n"))
		return
	}
	c.server.conns[c] = struct{}{}
	c.server.mutex.Unlock()
	defer func() {
		c.server.mutex.Lock()
		delete(c.server.conns, c)
		c.server.mutex.Unlock()
	}()

	var (
		greetingData *GreetingData
		err          error
	)
	c.session, greetingData, err = c.server.options.NewSession(c)
	if err != nil {
		var (
			resp    *imap.StatusResponse
			imapErr *imap.Error
		)
		if errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeBye {
			resp = (*imap.StatusResponse)(imapErr)
		} else {
			c.server.logger().Printf("failed to create session (remote %v): %v", c.conn.RemoteAddr(), err)
			resp = internalServerErrorResp
		}
		if err := c.writeStatusResp("", resp); err != nil {
			if !isConnectionClosedError(err) {
				c.server.logger().Printf("failed to write greeting (remote %v): %v", c.conn.RemoteAddr(), err)
			}
		}
		return
	}

	defer func() {
		if c.session != nil {
			if err := c.session.Close(); err != nil {
				if !isConnectionClosedError(err) {
					c.server.logger().Printf("failed to close session (remote %v): %v", c.conn.RemoteAddr(), err)
				}
			}
		}
	}()

	// Stop the NOTIFY pump (if any) before the session is closed: deferred
	// calls run in LIFO order, so this executes first on teardown.
	defer c.stopNotifyPump()

	// Capabilities that depend on optional session interfaces (IMAP4rev2,
	// NAMESPACE, MOVE, UNAUTHENTICATE, ...) are advertised by availableCaps only
	// when the session implements them, and each command handler returns a clean
	// error if invoked without support. The server therefore degrades gracefully
	// when configured with a capability its session does not implement, rather
	// than failing every connection. Operators that require a capability should
	// assert it at compile time, e.g.:
	//   var _ imapserver.SessionIMAP4rev2 = (*mySession)(nil)
	c.state = imap.ConnStateNotAuthenticated
	statusType := imap.StatusResponseTypeOK
	if greetingData != nil && greetingData.PreAuth {
		c.state = imap.ConnStateAuthenticated
		statusType = imap.StatusResponseTypePreAuth
	}
	if err := c.writeCapabilityStatus("", statusType, "IMAP server ready"); err != nil {
		if !isConnectionClosedError(err) {
			c.server.logger().Printf("failed to write greeting (remote %v): %v", c.conn.RemoteAddr(), err)
		}
		return
	}

	// byeOnExit is set when the loop ends because Server.Shutdown asked it to,
	// at a point where nothing is in flight. Every other way out -- LOGOUT,
	// client EOF, a protocol error -- has already said what it needed to say.
	byeOnExit := false
	for {
		var readTimeout time.Duration
		switch c.state {
		case imap.ConnStateAuthenticated, imap.ConnStateSelected:
			readTimeout = idleReadTimeout
		default:
			readTimeout = cmdReadTimeout
		}
		c.setReadTimeout(readTimeout)

		dec := imapwire.NewDecoder(c.br, imapwire.ConnSideServer)
		dec.MaxSize = maxCommandSize
		dec.CheckBufferedLiteralFunc = c.checkBufferedLiteral

		dec.QuotedUTF8 = c.useQuotedUTF8()

		if c.state == imap.ConnStateLogout {
			break
		}

		// Between commands: nothing is in flight, so a shutdown request is
		// honoured immediately, and one that arrives while we are blocked
		// below wakes the read and is honoured on the way out.
		if !c.setIdle() {
			byeOnExit = true
			break
		}
		eof := dec.EOF()
		if !c.setActive() {
			byeOnExit = !eof
			break
		}
		if eof {
			break
		}

		c.setReadTimeout(cmdReadTimeout)
		if err := c.readCommand(dec); err != nil {
			if errors.Is(err, errShutdown) {
				byeOnExit = true
				break
			}
			var imapErr *imap.Error
			if !isConnectionClosedError(err) && !(errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeBye) {
				c.server.logger().Printf("failed to read command (remote %v): %v", c.conn.RemoteAddr(), err)
			}
			break
		}
	}

	if byeOnExit {
		// The NOTIFY pump writes unsolicited responses from its own goroutine,
		// independent of this loop. BYE announces that the server is done
		// talking (RFC 9051 §7.1.5), so the pump must be gone before the BYE
		// goes out -- not stopped in the deferred teardown, which only runs
		// after the lingering close. Left running, a pump write during the
		// linger would hit the half-closed socket, fail, and abort the
		// connection hard, defeating the clean close. stopNotifyPump is
		// idempotent, so the deferred call is then a no-op.
		c.stopNotifyPump()
		c.shutdownBye()
	}
}

// shutdownLingerTimeout bounds how long shutdownBye waits for the client to
// close its side after the BYE. Same idea and order of magnitude as net/http's
// rstAvoidanceDelay.
const shutdownLingerTimeout = 500 * time.Millisecond

// shutdownBye announces the shutdown (RFC 9051 §7.1.5) and closes the
// connection cleanly.
//
// Cleanly is the operative word. When a command races the shutdown it is
// dropped unread, and closing a socket with unread input in its receive queue
// sends RST rather than FIN. The client would then see the BYE followed by
// "connection reset" instead of EOF -- or on some stacks not see the BYE at
// all, since RST can discard data still in flight. So after the BYE we
// half-close, which hands the client a clean EOF right behind the BYE, then
// drain whatever it sent until it closes its side or the linger timeout runs
// out. Only then does the deferred Close in serve run, with nothing left in the
// receive queue to provoke an RST.
//
// Best effort throughout: the client may already be gone, every step is
// bounded by a deadline, and Shutdown's own context force-closes anything that
// outlives the grace period.
func (c *Conn) shutdownBye() {
	if err := c.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeBye,
		Text: "Server shutting down",
	}); err != nil {
		if !isConnectionClosedError(err) {
			c.server.logger().Printf("failed to write shutdown BYE (remote %v): %v", c.conn.RemoteAddr(), err)
		}
		return
	}
	c.lingerClose()
}

// finishBye ends the connection on a BYE-typed error, and is a no-op on any
// other one (err comes back unchanged either way, so callers can `return
// c.finishBye(err)` without inspecting it first).
//
// A BYE-typed error is the backend saying this connection is over — the session
// lost the mailbox under it (a UIDVALIDITY change, a deletion), or the server is
// refusing to keep talking. BYE is untagged by definition (RFC 9051 §7.1.5), so
// it cannot double as a command's completion: writing it with the tag produces
// `a7 BYE …`, which is not a valid tagged response, and — because that reads as
// an ordinary completion — left the connection open on a session that then
// refused every command. Say it the way the protocol spells it, linger so the
// close behind it is a clean EOF rather than an RST, and stop reading.
//
// This is the convention NewSession's BYE already follows (see serve), and the
// serve loop already expects a BYE-typed error to come back out of readCommand:
// it skips the "failed to read command" log for exactly this case.
func (c *Conn) finishBye(err error) error {
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBye {
		return err
	}
	if writeErr := c.writeStatusResp("", (*imap.StatusResponse)(imapErr)); writeErr != nil {
		return writeErr
	}
	c.lingerClose()
	return err
}

// lingerClose half-closes the write side and drains whatever the client has
// already sent, so the Close that follows delivers a clean EOF behind the bytes
// just written rather than an RST that can discard them. See shutdownBye for
// the full argument; every announced end-of-connection wants this treatment,
// not just the shutdown one. Best effort, bounded by shutdownLingerTimeout.
func (c *Conn) lingerClose() {
	// Read c.conn under the mutex: STARTTLS reassigns it and forceCloseConns
	// closes it from other goroutines.
	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()

	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	conn.SetReadDeadline(time.Now().Add(shutdownLingerTimeout))
	io.Copy(io.Discard, c.br)
}

func (c *Conn) readCommand(dec *imapwire.Decoder) (err error) {
	defer func() {
		if decErr := dec.Err(); decErr != nil && strings.Contains(decErr.Error(), "max size exceeded") {
			_ = c.writeStatusResp("", &imap.StatusResponse{
				Type: imap.StatusResponseTypeBye,
				Text: "Command too long",
			})
			err = fmt.Errorf("command exceeded MaxSize")
		}
	}()
	for {
		if dec.EOF() {
			return nil
		}

		// Use non-destructive CRLF() instead of ExpectCRLF() to avoid
		// setting decoder error when we encounter non-empty lines.
		// If this fails (not a CRLF), we break and parse the command.
		if dec.CRLF() {
			continue
		}
		break
	}

	var tag, name string
	if !dec.ExpectAtom(&tag) || !dec.ExpectSP() || !dec.ExpectAtom(&name) {
		return fmt.Errorf("in command: %w", dec.Err())
	}
	name = strings.ToUpper(name)

	numKind := NumKindSeq
	if name == "UID" {
		numKind = NumKindUID
		var subName string
		if !dec.ExpectSP() || !dec.ExpectAtom(&subName) {
			return fmt.Errorf("in command: %w", dec.Err())
		}
		name = "UID " + strings.ToUpper(subName)
	}

	// Record the in-progress command so the NOTIFY pump can defer
	// EXPUNGE/VANISHED updates while commands that forbid them run (see
	// UpdateWriter.ExpungeAllowed).
	c.setActiveCommand(name)
	defer c.setActiveCommand("")

	// TODO: handle multiple commands concurrently
	sendOK := true
	switch name {
	case "NOOP", "CHECK":
		err = c.handleNoop(dec)
	case "LOGOUT":
		err = c.handleLogout(dec)
	case "CAPABILITY":
		err = c.handleCapability(dec)
	case "ID":
		err = c.handleID(tag, dec)
		sendOK = false
	case "STARTTLS":
		err = c.handleStartTLS(tag, dec)
		sendOK = false
	case "AUTHENTICATE":
		err = c.handleAuthenticate(tag, dec)
		sendOK = false
	case "UNAUTHENTICATE":
		err = c.handleUnauthenticate(dec)
	case "LOGIN":
		err = c.handleLogin(tag, dec)
		sendOK = false
	case "ENABLE":
		err = c.handleEnable(dec)
	case "CREATE":
		err = c.handleCreate(dec)
	case "DELETE":
		err = c.handleDelete(dec)
	case "RENAME":
		err = c.handleRename(dec)
	case "SUBSCRIBE":
		err = c.handleSubscribe(dec)
	case "UNSUBSCRIBE":
		err = c.handleUnsubscribe(dec)
	case "STATUS":
		err = c.handleStatus(dec)
	case "LIST":
		err = c.handleList(dec)
	case "LSUB":
		err = c.handleLSub(dec)
	case "NAMESPACE":
		err = c.handleNamespace(dec)
	case "GETACL":
		err = c.handleGetACL(dec)
	case "SETACL":
		err = c.handleSetACL(dec)
	case "DELETEACL":
		err = c.handleDeleteACL(dec)
	case "LISTRIGHTS":
		err = c.handleListRights(dec)
	case "MYRIGHTS":
		err = c.handleMyRights(dec)
	case "IDLE":
		err = c.handleIdle(dec)
	case "NOTIFY":
		err = c.handleNotify(tag, dec)
		sendOK = false
	case "SELECT", "EXAMINE":
		err = c.handleSelect(tag, dec, name == "EXAMINE")
		sendOK = false
	case "CLOSE", "UNSELECT":
		err = c.handleUnselect(dec, name == "CLOSE")
	case "APPEND":
		err = c.handleAppend(tag, dec)
		sendOK = false
	case "FETCH", "UID FETCH":
		err = c.handleFetch(dec, numKind)
	case "EXPUNGE":
		err = c.handleExpunge(dec)
	case "UID EXPUNGE":
		err = c.handleUIDExpunge(dec)
	case "STORE", "UID STORE":
		err = c.handleStore(dec, numKind)
	case "COPY", "UID COPY":
		err = c.handleCopy(tag, dec, numKind)
		sendOK = false
	case "MOVE", "UID MOVE":
		err = c.handleMove(dec, numKind)
	case "SEARCH", "UID SEARCH":
		err = c.handleSearch(tag, dec, numKind)
	case "SORT", "UID SORT":
		err = c.handleSort(tag, dec, numKind)
	case "GETMETADATA":
		err = c.handleGetMetadata(tag, dec)
		sendOK = false
	case "SETMETADATA":
		err = c.handleSetMetadata(dec)
	case "ESEARCH":
		// RFC 7377: ESEARCH always returns UIDs (message numbers are meaningless
		// for unselected mailboxes), so there is no "UID ESEARCH" form.
		err = c.handleESearch(tag, dec)
	case "THREAD", "UID THREAD":
		err = c.handleThread(dec, numKind)
	default:
		if c.state == imap.ConnStateNotAuthenticated {
			// Don't allow a single unknown command before authentication to
			// mitigate cross-protocol attacks:
			// https://www-archive.mozilla.org/projects/netlib/portbanning
			c.state = imap.ConnStateLogout
			defer c.Bye("Unknown command")
		}
		err = &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Unknown command",
		}
	}

	// A handler that was parked in an idle wait when Shutdown arrived has no
	// tagged response to give; the serve loop answers with BYE.
	if errors.Is(err, errShutdown) {
		return err
	}

	dec.DiscardLine()

	// A command whose remaining octets could not be skipped leaves the stream
	// pointing into client data rather than at a command boundary: whatever
	// follows would be executed as commands. There is no way back from that, so
	// end the connection instead of continuing to parse (RFC 9051 §2.2.1).
	if dec.Desynchronized() {
		_ = c.writeStatusResp("", &imap.StatusResponse{
			Type: imap.StatusResponseTypeBye,
			Text: "Unable to resynchronize command stream",
		})
		return fmt.Errorf("imapserver: command stream desynchronized")
	}

	var (
		resp    *imap.StatusResponse
		imapErr *imap.Error
		decErr  *imapwire.DecoderExpectError
	)
	if errors.As(err, &imapErr) {
		if imapErr.Type == imap.StatusResponseTypeBye {
			return c.finishBye(err)
		}
		resp = (*imap.StatusResponse)(imapErr)
		// An OK-typed error is a successful completion that carries a response
		// code (e.g. MODIFIED from a conditional STORE, RFC 7162 §3.1.3), not a
		// failure. Flush pending mailbox updates before the tagged line, as the
		// nil-error path below does — for MODIFIED in particular the pending
		// unsolicited FETCH is often the very flag change that made the STORE
		// fail its precondition.
		if imapErr.Type == imap.StatusResponseTypeOK {
			if err := c.poll(name); err != nil {
				return c.finishBye(err)
			}
		}
	} else if errors.As(err, &decErr) {
		resp = &imap.StatusResponse{
			Type: imap.StatusResponseTypeBad,
			Code: imap.ResponseCodeClientBug,
			Text: "Syntax error: " + decErr.Message,
		}
	} else if err != nil {
		c.server.logger().Printf("handling %v command (remote %v): %v", name, c.conn.RemoteAddr(), err)
		resp = internalServerErrorResp
	} else {
		if !sendOK {
			return nil
		}
		// The command-boundary poll is where a backend most often discovers the
		// session is finished (the selected mailbox changed UIDVALIDITY or was
		// deleted), and its error bypasses the mapping above — so route it
		// through finishBye too, or a BYE there reaches the client as a bare EOF.
		if err := c.poll(name); err != nil {
			return c.finishBye(err)
		}
		resp = &imap.StatusResponse{
			Type: imap.StatusResponseTypeOK,
			Text: fmt.Sprintf("%v completed", name),
		}
	}
	return c.writeStatusResp(tag, resp)
}

func (c *Conn) handleNoop(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	return nil
}

func (c *Conn) handleLogout(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	c.state = imap.ConnStateLogout

	return c.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeBye,
		Text: "Logging out",
	})
}

func (c *Conn) handleDelete(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Delete(c.ctx, name)
}

func (c *Conn) handleRename(dec *imapwire.Decoder) error {
	var oldName, newName string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&oldName) || !dec.ExpectSP() || !dec.ExpectMailbox(&newName) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	var options imap.RenameOptions
	return c.session.Rename(c.ctx, &RenameWriter{conn: c}, oldName, newName, &options)
}

// RenameWriter writes the unsolicited responses that MAY accompany a successful
// RENAME. Per RFC 9051 §6.3.6, a server SHOULD send an untagged LIST response
// carrying the OLDNAME extended data item so that IMAP4rev2 clients learn the
// mailbox's new name.
type RenameWriter struct {
	conn *Conn
}

// WriteOldName sends `* LIST (attrs) delim newName ("OLDNAME" (oldName))`, where
// data.OldName holds the previous name. It is a no-op unless the session is being
// served IMAP4rev2 semantics — rev2 was enabled, OR the server does not advertise
// IMAP4rev1 at all, in which case no ENABLE is needed. OLDNAME is an IMAP4rev2 /
// LIST-EXTENDED return-data item and an unsolicited extended LIST would confuse an
// IMAP4rev1-only client (this mirrors how RECENT is suppressed for rev2 clients).
func (w *RenameWriter) WriteOldName(data *imap.ListData) error {
	if !w.conn.isIMAP4rev2() {
		return nil
	}
	return w.conn.writeListData(data, nil, true)
}

func (c *Conn) handleSubscribe(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Subscribe(c.ctx, name)
}

func (c *Conn) handleUnsubscribe(dec *imapwire.Decoder) error {
	var name string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&name) || !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	return c.session.Unsubscribe(c.ctx, name)
}

func (c *Conn) checkBufferedLiteral(size int64, nonSync bool) error {
	if size > 4096 {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "Literals are limited to 4096 bytes for this command",
		}
	}

	return c.acceptLiteral(size, nonSync)
}

func (c *Conn) acceptLiteral(size int64, nonSync bool) error {
	if nonSync && size > 4096 && !c.server.options.caps().Has(imap.CapLiteralPlus) {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Non-synchronizing literals are limited to 4096 bytes",
		}
	}

	if nonSync {
		return nil
	}

	return c.writeContReq("Ready for literal data")
}

func (c *Conn) canAuth() bool {
	if c.state != imap.ConnStateNotAuthenticated {
		return false
	}

	// Allow custom TLS detection (e.g., for reverse proxy setups)
	if c.server.options.IsTLS != nil {
		return c.server.options.IsTLS(c.conn) || c.server.options.InsecureAuth
	}

	// Default: detect TLS via type assertion
	_, isTLS := c.conn.(*tls.Conn)
	return isTLS || c.server.options.InsecureAuth
}

func (c *Conn) writeStatusResp(tag string, statusResp *imap.StatusResponse) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeStatusResp(enc.Encoder, tag, statusResp)
}

func (c *Conn) writeContReq(text string) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeContReq(enc.Encoder, text)
}

func (c *Conn) writeCapabilityStatus(tag string, typ imap.StatusResponseType, text string) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeCapabilityStatus(enc.Encoder, tag, typ, c.availableCaps(), text)
}

func (c *Conn) checkState(state imap.ConnState) error {
	if state == imap.ConnStateAuthenticated && c.state == imap.ConnStateSelected {
		return nil
	}
	if c.state != state {
		return newClientBugError(fmt.Sprintf("This command is only valid in the %s state", state))
	}
	return nil
}

// checkWritableMailbox rejects a command that would change the selected
// mailbox when it was selected read-only.
//
// RFC 9051 §6.3.2: EXAMINE selects a mailbox read-only, and "no changes to the
// permanent state of the mailbox, including per-user state, are permitted".
// Enforcing it here rather than in the session keeps the backend out of a
// decision it cannot make correctly: it is not told whether an Expunge call
// came from the client or from the implicit expunge on CLOSE, which RFC 3501
// §6.4.2 requires to be silently skipped instead.
func (c *Conn) checkWritableMailbox() error {
	if err := c.checkState(imap.ConnStateSelected); err != nil {
		return err
	}
	if c.selectedReadOnly {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Mailbox is read-only",
		}
	}
	return nil
}

func (c *Conn) setReadTimeout(dur time.Duration) {
	if dur > 0 {
		c.conn.SetReadDeadline(time.Now().Add(dur))
	} else {
		c.conn.SetReadDeadline(time.Time{})
	}
}

func (c *Conn) setWriteTimeout(dur time.Duration) {
	if dur > 0 {
		c.conn.SetWriteDeadline(time.Now().Add(dur))
	} else {
		c.conn.SetWriteDeadline(time.Time{})
	}
}

// expungeReportingCmd reports whether the command's own tagged OK must be
// preceded by an untagged EXPUNGE for each removed message (RFC 9051 §6.4.3,
// RFC 4315 §2.1 for the UID variant). CLOSE is deliberately absent: it removes
// messages without reporting them.
func expungeReportingCmd(cmd string) bool {
	switch cmd {
	case "EXPUNGE", "UID EXPUNGE":
		return true
	default:
		return false
	}
}

func (c *Conn) poll(cmd string) error {
	switch c.state {
	case imap.ConnStateAuthenticated, imap.ConnStateSelected:
		// nothing to do
	default:
		return nil
	}

	// RFC 5465 §3.1: once NOTIFY has been used, message events for the selected
	// mailbox are reported only when the client asked for them with the
	// SELECTED/SELECTED-DELAYED specifier. Omitting that specifier "is the same
	// as specifying SELECTED NONE", and NOTIFY NONE disables everything — in
	// both cases the pending updates must stay queued rather than be flushed at
	// this sync point, which is what makes NOTIFY SET ... NONE usable as the
	// snapshot facility of RFC 5465 §5 and what keeps sequence numbers stable
	// for clients that rely on it.
	//
	// The filter covers notifications only. EXPUNGE and UID EXPUNGE MUST report
	// each removed message with an untagged EXPUNGE before their tagged OK (RFC
	// 9051 §6.4.3, RFC 4315 §2.1); that is command response data, which no
	// NOTIFY setting suppresses. Flushing then necessarily also delivers
	// whatever else the queue holds: updates are sequence-numbered, so a later
	// EXPUNGE cannot be reported without the earlier ones that shift the
	// numbering.
	if !expungeReportingCmd(cmd) && !c.notifySelectedMessageEventsEnabled() {
		return nil
	}

	// EXPUNGE renumbers the sequence space, so it must not be delivered by the
	// post-command poll of a command that referenced messages by sequence
	// number (RFC 3501 §5.5): FETCH, STORE, SEARCH and their SORT/THREAD
	// extensions. The UID variants ("UID FETCH", …) key on stable UIDs and are
	// exempt, so matching the bare names is correct.
	allowExpunge := true
	switch cmd {
	case "FETCH", "STORE", "SEARCH", "SORT", "THREAD":
		allowExpunge = false
	}

	w := &UpdateWriter{conn: c, allowExpunge: allowExpunge}
	return c.session.Poll(c.ctx, w, allowExpunge)
}

// useQuotedUTF8 reports whether IMAP strings and mailbox names should be
// encoded and decoded as UTF-8 (RFC 9051 Net-Unicode) rather than the
// Modified UTF-7 of IMAP4rev1.
//
// UTF-8 is a backward-incompatible change from Modified UTF-7 for non-ASCII
// names, so per RFC 9051 Section 5.1 and the ENABLE handshake (RFC 5161) it
// only takes effect once the client has explicitly negotiated it via
// ENABLE IMAP4rev2 or ENABLE UTF8=ACCEPT. Gating on the advertised (rather
// than enabled) capability would send incompatible mailbox names to a legacy
// IMAP4rev1 client that never enabled IMAP4rev2.
//
// A server that does not advertise IMAP4rev1 has no legacy clients to protect,
// so UTF-8 applies unconditionally.
//
// This is isIMAP4rev2() plus the UTF8=ACCEPT term, but it is spelled out rather
// than calling that helper: this function takes c.mutex and reads c.enabled
// directly, so calling the (locking) helper from inside it would deadlock.
func (c *Conn) useQuotedUTF8() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.enabled.Has(imap.CapIMAP4rev2) ||
		c.enabled.Has(imap.CapUTF8Accept) ||
		!c.server.options.caps().Has(imap.CapIMAP4rev1)
}

type responseEncoder struct {
	*imapwire.Encoder
	conn *Conn
}

func newResponseEncoder(conn *Conn) *responseEncoder {
	wireEnc := imapwire.NewEncoder(conn.bw, imapwire.ConnSideServer)
	wireEnc.QuotedUTF8 = conn.useQuotedUTF8()

	conn.encMutex.Lock() // released by responseEncoder.end
	conn.setWriteTimeout(respWriteTimeout)
	return &responseEncoder{
		Encoder: wireEnc,
		conn:    conn,
	}
}

func (enc *responseEncoder) end() {
	if enc.Encoder == nil {
		panic("imapserver: responseEncoder.end called twice")
	}
	enc.Encoder = nil
	enc.conn.setWriteTimeout(0)
	enc.conn.encMutex.Unlock()
}

func (enc *responseEncoder) Literal(size int64) io.WriteCloser {
	enc.conn.setWriteTimeout(literalWriteTimeout)
	return literalWriter{
		WriteCloser: enc.Encoder.Literal(size, nil),
		conn:        enc.conn,
	}
}

type literalWriter struct {
	io.WriteCloser
	conn *Conn
}

func (lw literalWriter) Close() error {
	lw.conn.setWriteTimeout(respWriteTimeout)
	return lw.WriteCloser.Close()
}

func writeStatusResp(enc *imapwire.Encoder, tag string, statusResp *imap.StatusResponse) error {
	if tag == "" {
		tag = "*"
	}
	enc.Atom(tag).SP().Atom(string(statusResp.Type)).SP()
	if statusResp.Code != "" {
		enc.Atom(fmt.Sprintf("[%v]", statusResp.Code)).SP()
	}
	enc.Text(statusResp.Text)
	return enc.CRLF()
}

func writeCapabilityOK(enc *imapwire.Encoder, tag string, caps []imap.Cap, text string) error {
	return writeCapabilityStatus(enc, tag, imap.StatusResponseTypeOK, caps, text)
}

func writeCapabilityStatus(enc *imapwire.Encoder, tag string, typ imap.StatusResponseType, caps []imap.Cap, text string) error {
	if tag == "" {
		tag = "*"
	}

	enc.Atom(tag).SP().Atom(string(typ)).SP().Special('[').Atom("CAPABILITY")
	for _, c := range caps {
		enc.SP().Atom(string(c))
	}
	enc.Special(']').SP().Text(text)
	return enc.CRLF()
}

func writeContReq(enc *imapwire.Encoder, text string) error {
	return enc.Atom("+").SP().Text(text).CRLF()
}

func newClientBugError(text string) error {
	return &imap.Error{
		Type: imap.StatusResponseTypeBad,
		Code: imap.ResponseCodeClientBug,
		Text: text,
	}
}

func (c *Conn) writeExists(numMessages uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeExists(enc.Encoder, numMessages)
}

func writeExists(enc *imapwire.Encoder, numMessages uint32) error {
	return enc.Atom("*").SP().Number(numMessages).SP().Atom("EXISTS").CRLF()
}

func (c *Conn) writeObsoleteRecent(n uint32) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	return writeObsoleteRecent(enc.Encoder, n)
}

func writeObsoleteRecent(enc *imapwire.Encoder, n uint32) error {
	return enc.Atom("*").SP().Number(n).SP().Atom("RECENT").CRLF()
}

func (c *Conn) setActiveCommand(name string) {
	c.cmdMutex.Lock()
	c.activeCmd = name
	c.cmdMutex.Unlock()
}

// notifyExpungeAllowed reports whether an asynchronous EXPUNGE/VANISHED
// update may be sent right now: RFC 3501 §5.5 / RFC 9051 §5.5 forbid sending
// EXPUNGE while a command that references messages by sequence number is in
// progress, because expunging renumbers the sequence space underneath it. That
// is FETCH, STORE and SEARCH, plus their SORT and THREAD extensions (all of
// which emit sequence-numbered responses); the UID variants are exempt as they
// key on stable UIDs.
func (c *Conn) notifyExpungeAllowed() bool {
	c.cmdMutex.Lock()
	defer c.cmdMutex.Unlock()
	switch c.activeCmd {
	case "FETCH", "STORE", "SEARCH", "SORT", "THREAD":
		return false
	default:
		return true
	}
}

// setNotifySelectedEvents records the message events the client requested for
// the selected mailbox with the SELECTED/SELECTED-DELAYED specifier. options is
// nil for NOTIFY NONE. It is called by the NOTIFY handler once the backend has
// accepted the new watch.
func (c *Conn) setNotifySelectedEvents(options *imap.NotifyOptions, fetchWriterOpts *fetchWriterOptions) {
	events := make(map[imap.NotifyEvent]bool)
	if options != nil {
		for _, item := range options.Items {
			if item.MailboxSpec != imap.NotifyMailboxSpecSelected &&
				item.MailboxSpec != imap.NotifyMailboxSpecSelectedDelayed {
				continue
			}
			for _, event := range item.Events {
				events[event] = true
			}
		}
	}

	c.notifyMutex.Lock()
	c.notifyUsed = true
	c.notifySelectedEvents = events
	c.notifyFetchWriterOptions = fetchWriterOpts
	c.notifyMutex.Unlock()
}

// resetNotifySelectedEvents restores the pre-NOTIFY behaviour. It is called
// when the session is torn down (UNAUTHENTICATE), since the watch belongs to
// the authenticated session rather than to the connection.
func (c *Conn) resetNotifySelectedEvents() {
	c.notifyMutex.Lock()
	c.notifyUsed = false
	c.notifySelectedEvents = nil
	c.notifyFetchWriterOptions = nil
	c.notifyMutex.Unlock()
}

// notifySelectedEventEnabled reports whether the given message event may be
// reported for the selected mailbox (RFC 5465 §3.1). It is always true until
// the client uses NOTIFY for the first time.
func (c *Conn) notifySelectedEventEnabled(event imap.NotifyEvent) bool {
	c.notifyMutex.Lock()
	defer c.notifyMutex.Unlock()
	if !c.notifyUsed {
		return true
	}
	return c.notifySelectedEvents[event]
}

// notifySelectedMessageEventsEnabled reports whether any message event at all
// may be reported for the selected mailbox.
func (c *Conn) notifySelectedMessageEventsEnabled() bool {
	c.notifyMutex.Lock()
	defer c.notifyMutex.Unlock()
	if !c.notifyUsed {
		return true
	}
	for event := range c.notifySelectedEvents {
		if event.IsMessageEvent() {
			return true
		}
	}
	return false
}

// UpdateWriter writes status updates.
type UpdateWriter struct {
	conn         *Conn
	allowExpunge bool

	// notify is set for writers handed to SessionNotify: expunge delivery is
	// additionally gated on the command currently in progress (see
	// ExpungeAllowed).
	notify bool
}

// ExpungeAllowed reports whether EXPUNGE/VANISHED updates may be written in
// the current context.
//
// For writers passed to SessionNotify methods this is dynamic: it returns
// false while a command that forbids unsolicited expunges (FETCH, STORE,
// SEARCH) is in progress on the connection. Backends running a NOTIFY watch
// should consult it once per delivery batch and withhold expunge updates
// (e.g. by passing it as the allowExpunge argument of SessionTracker.Poll)
// when it reports false; withheld updates are delivered at the next sync
// point. The check is advisory: a command may start between the check and the
// write, which is the same ordering ambiguity a single-threaded server has
// when a command arrives while an update is being written.
func (w *UpdateWriter) ExpungeAllowed() bool {
	if !w.allowExpunge {
		return false
	}
	if w.notify {
		return w.conn.notifyExpungeAllowed()
	}
	return true
}

// DelayedExpungeAllowed reports whether an expunge withheld by the
// SELECTED-DELAYED specifier may be released now.
//
// RFC 5465 §6.1.2 delays MessageExpunge "until the client issues a command that
// allows returning information about expunged messages ... for example, till a
// NOOP or an IDLE command has been issued". This reports true while such a
// command is in progress, so a backend can release its delayed expunges from
// NotifyPoll instead of leaving them queued until the command ends — which for
// IDLE would mean holding them, and every update queued behind them, for the
// entire IDLE.
//
// Between commands it returns false: a client using SELECTED-DELAYED is
// entitled to stable sequence numbers until it asks.
func (w *UpdateWriter) DelayedExpungeAllowed() bool {
	if !w.ExpungeAllowed() {
		return false
	}
	w.conn.cmdMutex.Lock()
	defer w.conn.cmdMutex.Unlock()
	switch w.conn.activeCmd {
	case "NOOP", "IDLE", "CHECK", "EXPUNGE", "UID EXPUNGE":
		return true
	default:
		return false
	}
}

// CondStoreEnabled reports whether the client has become CONDSTORE-aware on
// this connection. Backends use it to decide whether STATUS notifications may
// carry HIGHESTMODSEQ (RFC 5465 §5.1, §5.2) and whether unsolicited FETCH
// responses may carry MODSEQ.
func (w *UpdateWriter) CondStoreEnabled() bool {
	return w.conn.supportsCondStore() && w.conn.CondStoreEnabled()
}

// WriteStatus writes an untagged STATUS response. It is used by NOTIFY
// (RFC 5465) to report message events in mailboxes other than the selected
// one. data must populate every field requested in options.
func (w *UpdateWriter) WriteStatus(data *imap.StatusData, options *imap.StatusOptions) error {
	return w.conn.writeStatus(data, options)
}

// WriteList writes an untagged LIST response. It is used by NOTIFY
// (RFC 5465) to report MailboxName and SubscriptionChange events: mailbox
// creation, deletion (with the \NonExistent attribute), renames (with the
// OLDNAME extended data item) and subscription changes.
//
// For a NOTIFY writer, OLDNAME is emitted regardless of ENABLE IMAP4rev2:
// RFC 5465 §5.4 requires the extended LIST (RFC 5258) carrying OLDNAME for
// renames, and a client that issued NOTIFY has, by using the extension,
// opted into these extended responses — so an IMAP4rev1 NOTIFY client must
// still learn a mailbox's new name. Non-NOTIFY callers keep the IMAP4rev2
// gate — isIMAP4rev2, so a rev2-only server counts even without ENABLE.
func (w *UpdateWriter) WriteList(data *imap.ListData) error {
	allowOldName := w.notify || w.conn.isIMAP4rev2()
	return w.conn.writeListData(data, nil, allowOldName)
}

// WriteMetadata writes an unsolicited METADATA response (RFC 5464 §4.4.2). It
// is used by NOTIFY (RFC 5465) to report MailboxMetadataChange events (with the
// mailbox name) and ServerMetadataChange events (with an empty mailbox name).
//
// entries lists the names of the changed annotations. Values are deliberately
// not part of the API: RFC 5464 §4.4 requires that "unsolicited METADATA
// responses MUST only contain entry names, not the values" — a client that
// wants the new value retrieves it with GETMETADATA. Deleted entries are
// reported like any other change (RFC 5465 §5.6, §5.7). Writing an empty list
// is a no-op.
func (w *UpdateWriter) WriteMetadata(mailbox string, entries []string) error {
	return w.conn.writeMetadataEntryList(mailbox, entries)
}

// WriteNotificationOverflow writes an untagged OK response with the
// NOTIFICATIONOVERFLOW response code (RFC 5465 §5.8).
//
// A backend calls this from NotifyPoll when it is unable or unwilling to keep
// delivering the requested notifications. Per the RFC the server then behaves
// as if NOTIFY NONE had been received: the backend must clear its own watch
// state and return nil from NotifyPoll after writing the overflow response.
//
// The connection-level watch is dropped here, which also ends the suppression
// of message events for the selected mailbox: the connection returns to the
// pre-NOTIFY behaviour of RFC 5465 §3.1, where those events are reported while
// a command is being processed. That is a deliberate departure from a literal
// reading of "behave as if a NOTIFY NONE command had just been received" —
// staying frozen would leave the updates accumulated so far undeliverable and
// the client's sequence numbers permanently stale, with no way for the server
// to recover on its own. The client has been told its notifications are off,
// and it gets a consistent view instead of a frozen one.
func (w *UpdateWriter) WriteNotificationOverflow() error {
	if err := w.conn.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Code: imap.ResponseCodeNotificationOverflow,
		Text: "Notifications disabled",
	}); err != nil {
		return err
	}
	w.conn.resetNotifySelectedEvents()
	return nil
}

// FetchWriter returns a writer for unsolicited FETCH responses. It is used
// by NOTIFY (RFC 5465 §5.2) to send the fetch attributes requested for
// MessageNew events in the selected mailbox.
//
// The writer is configured from the MessageNew fetch-att list of the installed
// watch, so BODY/BODYSTRUCTURE are emitted and the obsolete RFC822 item names
// are reported the way the client spelled them.
func (w *UpdateWriter) FetchWriter() *FetchWriter {
	fw := &FetchWriter{conn: w.conn}
	w.conn.notifyMutex.Lock()
	if opts := w.conn.notifyFetchWriterOptions; opts != nil {
		fw.options = *opts
	}
	w.conn.notifyMutex.Unlock()
	return fw
}

// WriteOK writes an untagged OK response carrying only informational text
// (e.g. "Still here"). Servers use it as an IDLE keepalive so NAT mappings
// and activity-based idle checkers observe traffic during long IDLE waits
// (mirrors Dovecot's imap_idle_notify_interval behaviour).
func (w *UpdateWriter) WriteOK(text string) error {
	return w.conn.writeStatusResp("", &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: text,
	})
}

// WriteExpunge writes an EXPUNGE response.
func (w *UpdateWriter) WriteExpunge(seqNum uint32) error {
	if !w.allowExpunge {
		return fmt.Errorf("imapserver: EXPUNGE updates are not allowed in this context")
	}
	return w.conn.writeExpunge(seqNum)
}

// WriteVanished writes a VANISHED response (RFC 7162 §3.2.10) reporting that the
// messages with the given UIDs have been expunged. It is used instead of
// WriteExpunge for QRESYNC-enabled sessions.
func (w *UpdateWriter) WriteVanished(uids imap.UIDSet) error {
	if !w.allowExpunge {
		return fmt.Errorf("imapserver: EXPUNGE/VANISHED updates are not allowed in this context")
	}
	return w.conn.writeVanished(uids, false)
}

// WriteNumMessages writes an EXISTS response.
func (w *UpdateWriter) WriteNumMessages(n uint32) error {
	return w.conn.writeExists(n)
}

// WriteNumRecent writes an RECENT response (not used in IMAP4rev2, will be ignored).
func (w *UpdateWriter) WriteNumRecent(n uint32) error {
	if w.conn.isIMAP4rev2() {
		return nil
	}
	return w.conn.writeObsoleteRecent(n)
}

// WriteMailboxFlags writes a FLAGS response.
func (w *UpdateWriter) WriteMailboxFlags(flags []imap.Flag) error {
	return w.conn.writeFlags(flags)
}

// WriteMessageFlags writes a FETCH response with FLAGS.
//
// modSeq is the modification sequence (RFC 7162) of the flag change. When non-zero
// and the session is CONDSTORE-aware, it is included as a MODSEQ data item so the
// client can advance its per-message modseq from the unsolicited update rather than
// falling back to a full re-sync (RFC 7162 §3.2). A zero modSeq is omitted.
func (w *UpdateWriter) WriteMessageFlags(seqNum uint32, uid imap.UID, flags []imap.Flag, modSeq uint64) error {
	// RFC 5465 §3.1: a client that used NOTIFY only receives the message events
	// it asked for with the SELECTED/SELECTED-DELAYED specifier. Dropping an
	// unrequested flag update is safe (unlike EXISTS/EXPUNGE, it carries no
	// sequence-number change), so the whole tracker queue can still be drained
	// while FlagChange stays filtered out.
	if !w.conn.notifySelectedEventEnabled(imap.NotifyEventFlagChange) {
		return nil
	}

	fetchWriter := &FetchWriter{conn: w.conn}
	respWriter := fetchWriter.CreateMessage(seqNum)
	if uid != 0 {
		respWriter.WriteUID(uid)
	}
	respWriter.WriteFlags(flags)
	// WriteModSeq itself drops the item for sessions that are not
	// CONDSTORE-aware (RFC 7162 §3.2); only the zero value is guarded here.
	if modSeq != 0 {
		respWriter.WriteModSeq(modSeq)
	}
	return respWriter.Close()
}
