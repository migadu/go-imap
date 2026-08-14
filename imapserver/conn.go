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

	// selectedReadOnly is true when the selected mailbox is read-only, either
	// because it was opened with EXAMINE or because the session reported it
	// read-only (RFC 4314 §5.2). Only touched from the command-loop goroutine,
	// like state.
	selectedReadOnly bool
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

// Context returns the connection's context. It is cancelled when the
// connection is torn down — by client disconnect, by the serve goroutine
// exiting, or by server shutdown. Backends may use it to bound blocking work,
// though blocking Session methods already receive it as their first argument.
func (c *Conn) Context() context.Context {
	return c.ctx
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

		if c.state == imap.ConnStateLogout || dec.EOF() {
			break
		}

		c.setReadTimeout(cmdReadTimeout)
		if err := c.readCommand(dec); err != nil {
			var imapErr *imap.Error
			if !isConnectionClosedError(err) && !(errors.As(err, &imapErr) && imapErr.Type == imap.StatusResponseTypeBye) {
				c.server.logger().Printf("failed to read command (remote %v): %v", c.conn.RemoteAddr(), err)
			}
			break
		}
	}
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

	dec.DiscardLine()

	var (
		resp    *imap.StatusResponse
		imapErr *imap.Error
		decErr  *imapwire.DecoderExpectError
	)
	if errors.As(err, &imapErr) {
		resp = (*imap.StatusResponse)(imapErr)
		// An OK-typed error is a successful completion that carries a response
		// code (e.g. MODIFIED from a conditional STORE, RFC 7162 §3.1.3), not a
		// failure. Flush pending mailbox updates before the tagged line, as the
		// nil-error path below does — for MODIFIED in particular the pending
		// unsolicited FETCH is often the very flag change that made the STORE
		// fail its precondition.
		if imapErr.Type == imap.StatusResponseTypeOK {
			if err := c.poll(name); err != nil {
				return err
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
		if err := c.poll(name); err != nil {
			return err
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
// data.OldName holds the previous name. It is a no-op unless the client has
// enabled IMAP4rev2: OLDNAME is an IMAP4rev2 / LIST-EXTENDED return-data item and
// an unsolicited extended LIST would confuse an IMAP4rev1-only client (this
// mirrors how RECENT is suppressed for rev2 clients).
func (w *RenameWriter) WriteOldName(data *imap.ListData) error {
	if !w.conn.enabledHas(imap.CapIMAP4rev2) {
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

func (c *Conn) poll(cmd string) error {
	switch c.state {
	case imap.ConnStateAuthenticated, imap.ConnStateSelected:
		// nothing to do
	default:
		return nil
	}

	allowExpunge := true
	switch cmd {
	case "FETCH", "STORE", "SEARCH":
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
// so UTF-8 applies unconditionally (mirrors the gating in WriteNumRecent).
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

// UpdateWriter writes status updates.
type UpdateWriter struct {
	conn         *Conn
	allowExpunge bool
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
	if w.conn.enabledHas(imap.CapIMAP4rev2) || !w.conn.server.options.caps().Has(imap.CapIMAP4rev1) {
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
