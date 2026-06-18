package imapserver

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func (c *Conn) handleID(tag string, dec *imapwire.Decoder) error {
	idData, err := readID(dec)
	if err != nil {
		// Wrap with %w so the underlying *imap.Error / *imapwire.DecoderExpectError
		// type is preserved. Otherwise the dispatcher in conn.go cannot detect it
		// via errors.As and reports a misleading [SERVERBUG] for what is really a
		// client-side syntax error.
		return fmt.Errorf("in id: %w", err)
	}

	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	// Store client ID for potential use by the session or other connection logic.
	c.mutex.Lock()
	c.clientID = idData
	c.mutex.Unlock()

	var serverIDData *imap.IDData
	if idSess, ok := c.session.(SessionID); ok {
		serverIDData = idSess.ID(idData)
	}

	enc := newResponseEncoder(c)
	enc.Atom("*").SP().Atom("ID")

	if serverIDData == nil {
		enc.SP().NIL()
	} else {
		enc.SP().Special('(')
		isFirstKey := true
		if serverIDData.Name != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "name", serverIDData.Name)
		}
		if serverIDData.Version != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "version", serverIDData.Version)
		}
		if serverIDData.OS != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "os", serverIDData.OS)
		}
		if serverIDData.OSVersion != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "os-version", serverIDData.OSVersion)
		}
		if serverIDData.Vendor != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "vendor", serverIDData.Vendor)
		}
		if serverIDData.SupportURL != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "support-url", serverIDData.SupportURL)
		}
		if serverIDData.Address != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "address", serverIDData.Address)
		}
		if serverIDData.Date != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "date", serverIDData.Date)
		}
		if serverIDData.Command != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "command", serverIDData.Command)
		}
		if serverIDData.Arguments != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "arguments", serverIDData.Arguments)
		}
		if serverIDData.Environment != "" {
			addIDKeyValue(enc.Encoder, &isFirstKey, "environment", serverIDData.Environment)
		}
		if serverIDData.Raw != nil {
			stdKeys := map[string]struct{}{
				"name": {}, "version": {}, "os": {}, "os-version": {}, "vendor": {},
				"support-url": {}, "address": {}, "date": {}, "command": {},
				"arguments": {}, "environment": {},
			}
			for k, v := range serverIDData.Raw {
				if _, ok := stdKeys[strings.ToLower(k)]; !ok {
					addIDKeyValue(enc.Encoder, &isFirstKey, k, v)
				}
			}
		}
		enc.Special(')')
	}

	err = enc.CRLF()
	enc.end()
	if err != nil {
		return err
	}

	return c.writeStatusResp(tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "ID completed",
	})
}

func readID(dec *imapwire.Decoder) (*imap.IDData, error) {
	// RFC 2971 requires "ID" SP id_params_list, but some clients (e.g.
	// Open-Xchange) send a bare "ID" with no argument at all. Be liberal in
	// what we accept and treat a missing argument as NIL, replying with the
	// server's own ID instead of rejecting the command.
	if !dec.SP() {
		return nil, nil
	}

	// Check for NIL without using ExpectNIL which leaves decoder in error state
	// Use Atom() instead of ExpectAtom() to avoid setting error
	var nilCheck string
	if dec.Atom(&nilCheck) && nilCheck == "NIL" {
		return nil, nil
	}

	data := &imap.IDData{
		Raw: make(map[string]string),
	}
	currKey := ""
	var params []string // Track all parameters for error reporting
	err := dec.ExpectList(func() error {
		if currKey == "" {
			// Reading a key - must be a string (not NIL)
			var key string
			if !dec.String(&key) {
				// Preserve a real decoder error (e.g. malformed literal) so the
				// dispatcher reports a syntax error; otherwise the key was an
				// atom/NIL where a string is required — a client bug.
				if decErr := dec.Err(); decErr != nil {
					return fmt.Errorf("in id key-val list: %w", decErr)
				}
				return &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Code: imap.ResponseCodeClientBug,
					Text: fmt.Sprintf("ID: expected string key (received %d parameters: %v)", len(params), params),
				}
			}
			params = append(params, key)
			currKey = key
			return nil
		}

		// Reading a value - can be string or NIL
		var value string
		if !dec.ExpectNString(&value) {
			// First check if there's a decoder error (e.g., invalid token or leftover error from ExpectNIL)
			if decErr := dec.Err(); decErr != nil {
				return fmt.Errorf("in id key-val list: %w", decErr)
			}
			// If we have an orphaned key, provide a clear error
			if currKey != "" {
				return &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Code: imap.ResponseCodeClientBug,
					Text: fmt.Sprintf("ID: missing value for key %q (received %d parameters: %v)", currKey, len(params), params),
				}
			}
			return fmt.Errorf("in id key-val list: unexpected error")
		}

		params = append(params, value)

		lowerKey := strings.ToLower(currKey)
		data.Raw[lowerKey] = value

		switch lowerKey {
		case "name":
			data.Name = value
		case "version":
			data.Version = value
		case "os":
			data.OS = value
		case "os-version":
			data.OSVersion = value
		case "vendor":
			data.Vendor = value
		case "support-url":
			data.SupportURL = value
		case "address":
			data.Address = value
		case "date":
			data.Date = value
		case "command":
			data.Command = value
		case "arguments":
			data.Arguments = value
		case "environment":
			data.Environment = value
		default:
			// Unknown key, already stored in Raw
		}
		currKey = ""

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Validate that all keys have values (even number of items)
	if currKey != "" {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Code: imap.ResponseCodeClientBug,
			Text: fmt.Sprintf("ID: odd number of parameters, missing value for key %q (received %d parameters: %v)", currKey, len(params), params),
		}
	}

	return data, nil
}

func addIDKeyValue(enc *imapwire.Encoder, isFirstKey *bool, key, value string) {
	if *isFirstKey {
		enc.Quoted(key).SP().Quoted(value)
	} else {
		enc.SP().Quoted(key).SP().Quoted(value)
	}
	*isFirstKey = false
}

// SessionID is an interface for sessions that can provide server ID information.
type SessionID interface {
	// ID returns server information in response to a client ID command.
	// The client's ID information is provided if available.
	ID(clientID *imap.IDData) *imap.IDData
}
