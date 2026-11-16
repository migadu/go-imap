package imapserver

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/emersion/go-sasl"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func (c *Conn) handleAuthenticate(tag string, dec *imapwire.Decoder) error {
	var mech string
	if !dec.ExpectSP() || !dec.ExpectAtom(&mech) {
		return dec.Err()
	}
	mech = imapwire.ToUpperASCII(mech)

	var initialRespStr string
	hasInitialResp := dec.SP()
	if hasInitialResp && !dec.ExpectAtom(&initialRespStr) {
		return dec.Err()
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	var initialResp []byte
	if hasInitialResp {
		if initialRespStr == "=" {
			// A single pad character is a zero-length initial response
			// (RFC 9051 §6.2.2), and is valid only here. go-sasl takes nil
			// for no response, so it must be non-nil.
			initialResp = []byte{}
		} else {
			var err error
			initialResp, err = decodeSASL(initialRespStr)
			if err != nil {
				return err
			}
		}
	}

	if err := c.checkState(imap.ConnStateNotAuthenticated); err != nil {
		return err
	}
	if !c.canAuth() {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodePrivacyRequired,
			Text: "TLS is required to authenticate",
		}
	}

	var saslServer sasl.Server
	if authSess, ok := c.session.(SessionSASL); ok {
		var err error
		saslServer, err = authSess.Authenticate(mech)
		if err != nil {
			return err
		}
	} else {
		if mech != "PLAIN" {
			return &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Text: "SASL mechanism not supported",
			}
		}
		saslServer = sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && identity != username {
				return &imap.Error{
					Type: imap.StatusResponseTypeNo,
					Code: imap.ResponseCodeAuthorizationFailed,
					Text: "SASL identity not supported",
				}
			}
			return c.session.Login(c.ctx, username, password)
		})
	}

	// SASL work and client waits must not consume a response's write deadline.
	// Each challenge gets its own encoder, as does the final completion.
	resp := initialResp
	for {
		challenge, done, err := saslServer.Next(resp)
		if err != nil {
			return err
		} else if done {
			break
		}

		// A challenge is plain base64 (RFC 9051 §6.2.2), so an empty one is
		// sent as "+ " (RFC 4648 §10: BASE64("") = ""). The "=" of
		// internal.EncodeSASL marks a zero-length initial response only.
		if err := c.writeContReq(base64.StdEncoding.EncodeToString(challenge)); err != nil {
			return err
		}

		encodedResp, err := c.readSASLResponse(dec)
		if err != nil {
			return err
		} else if string(encodedResp) == "*" {
			return &imap.Error{
				Type: imap.StatusResponseTypeBad,
				Text: "AUTHENTICATE cancelled",
			}
		}

		resp, err = decodeSASL(string(encodedResp))
		if err != nil {
			return err
		}
	}

	c.state = imap.ConnStateAuthenticated
	return c.writeCapabilityStatus(tag, imap.StatusResponseTypeOK, fmt.Sprintf("%v authentication successful", mech))
}

// readSASLResponse reads the client's response to a continuation request,
// "a single line" (RFC 9051 §6.2.2), and returns it without its line ending.
//
// Only a complete line is a response. bufio.Reader.ReadLine is not used: when
// the read under it fails it returns the octets that did arrive as a line, with
// a nil error, so a response cut off by a timeout or a close would be taken for
// a whole one.
func (c *Conn) readSASLResponse(dec *imapwire.Decoder) ([]byte, error) {
	c.setReadTimeout(cmdReadTimeout)
	line, err := c.br.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// The response does not fit the read buffer. That is the client's
		// doing: skip the rest of the line so the next command is read from a
		// command boundary, and refuse the exchange.
		var prevTail string
		for err == bufio.ErrBufferFull {
			prevTail = retainLiteralTail(prevTail, line)
			line, err = c.br.ReadSlice('\n')
		}
		if err != nil {
			return nil, saslReadError(err)
		}
		tail := strings.TrimSuffix(strings.TrimSuffix(prevTail+string(line), "\n"), "\r")
		if imapwire.EndsWithNonSyncLiteral(tail) {
			dec.MarkDesynchronized()
		}
		return nil, newClientBugError("SASL response too long")
	} else if err != nil {
		return nil, saslReadError(err)
	}
	line = line[:len(line)-1]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if imapwire.EndsWithNonSyncLiteral(string(line)) {
		dec.MarkDesynchronized()
	}
	return line, nil
}

func retainLiteralTail(prev string, chunk []byte) string {
	const keep = 64
	if len(chunk) >= keep {
		return string(chunk[len(chunk)-keep:])
	}
	s := prev + string(chunk)
	if len(s) > keep {
		s = s[len(s)-keep:]
	}
	return s
}

// saslReadError ends the connection after a failure to read the client's SASL
// response. The client stopped mid-exchange, which is not a server failure, and
// the exchange cannot be answered: the input is no longer at a line boundary.
//
// A timeout is an inactivity autologout, announced with an untagged BYE (RFC
// 9051 §7.1.5) as handleIdle does. Any other read error -- the client closed or
// reset the connection -- goes back to the serve loop as one between commands
// would.
func saslReadError(err error) error {
	if isTimeoutError(err) {
		return &imap.Error{
			Type: imap.StatusResponseTypeBye,
			Text: "Timed out waiting for AUTHENTICATE response",
		}
	}
	return &clientReadError{err}
}

// decodeSASL decodes a client response, which is base64 (RFC 9051 §6.2.2).
// Invalid base64 is the client's error and gets a tagged BAD, not
// NO [SERVERBUG]. In a reply to a continuation request a zero-length response
// is an empty line; a lone "=" is not base64 and is refused.
func decodeSASL(s string) ([]byte, error) {
	// base64.StdEncoding.DecodeString silently ignores '\r' and '\n', so reject
	// any remaining CR/LF explicitly before decoding.
	if strings.ContainsAny(s, "\r\n") {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Malformed SASL response",
		}
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Malformed SASL response",
		}
	}
	return b, nil
}

func (c *Conn) handleUnauthenticate(dec *imapwire.Decoder) error {
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}
	session, ok := c.session.(SessionUnauthenticate)
	if !ok {
		return newClientBugError("UNAUTHENTICATE is not supported")
	}
	// Stop the NOTIFY pump BEFORE tearing down the backend's session state:
	// SessionNotify guarantees no NotifyPoll call is in flight when the watch
	// is torn down, and Unauthenticate drops that state. Stopping first also
	// means the pump can no longer write to a connection that is about to
	// re-enter the not-authenticated state.
	hadPump := c.notifyPumpRunning()
	c.stopNotifyPump()

	if err := c.flushClosingCondStore(); err != nil {
		c.restartNotifyPump(hadPump)
		return err
	}

	if err := session.Unauthenticate(c.ctx); err != nil {
		// The command failed, so the session — and with it the client's watch —
		// is still in effect. Put the pump back.
		c.restartNotifyPump(hadPump)
		return err
	}

	// The watch belongs to the authenticated session: a connection returning to
	// the not-authenticated state also returns to the pre-NOTIFY notification
	// behaviour of RFC 5465 §3.1.
	c.resetNotifySelectedEvents()
	c.state = imap.ConnStateNotAuthenticated
	c.mutex.Lock()
	c.enabled = make(imap.CapSet)
	// Reset CONDSTORE-enabled state too (RFC 8437): the connection returns to
	// not-authenticated as if freshly connected, so a re-authenticated client
	// that never re-enables CONDSTORE must not receive MODSEQ data items.
	c.condStore = false
	c.condStorePending = false
	c.mutex.Unlock()
	return nil
}
