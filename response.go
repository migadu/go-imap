package imap

import (
	"fmt"
	"strings"
)

// StatusResponseType is a generic status response type.
type StatusResponseType string

const (
	StatusResponseTypeOK      StatusResponseType = "OK"
	StatusResponseTypeNo      StatusResponseType = "NO"
	StatusResponseTypeBad     StatusResponseType = "BAD"
	StatusResponseTypePreAuth StatusResponseType = "PREAUTH"
	StatusResponseTypeBye     StatusResponseType = "BYE"
)

// ResponseCode is a response code.
type ResponseCode string

const (
	ResponseCodeAlert                ResponseCode = "ALERT"
	ResponseCodeAlreadyExists        ResponseCode = "ALREADYEXISTS"
	ResponseCodeAuthenticationFailed ResponseCode = "AUTHENTICATIONFAILED"
	ResponseCodeAuthorizationFailed  ResponseCode = "AUTHORIZATIONFAILED"
	ResponseCodeBadCharset           ResponseCode = "BADCHARSET"
	ResponseCodeCannot               ResponseCode = "CANNOT"
	ResponseCodeClientBug            ResponseCode = "CLIENTBUG"
	ResponseCodeContactAdmin         ResponseCode = "CONTACTADMIN"
	ResponseCodeCorruption           ResponseCode = "CORRUPTION"
	ResponseCodeExpired              ResponseCode = "EXPIRED"
	ResponseCodeHasChildren          ResponseCode = "HASCHILDREN"
	ResponseCodeInUse                ResponseCode = "INUSE"
	ResponseCodeLimit                ResponseCode = "LIMIT"
	ResponseCodeNonExistent          ResponseCode = "NONEXISTENT"
	ResponseCodeNoPerm               ResponseCode = "NOPERM"
	ResponseCodeOverQuota            ResponseCode = "OVERQUOTA"
	ResponseCodeParse                ResponseCode = "PARSE"
	ResponseCodePrivacyRequired      ResponseCode = "PRIVACYREQUIRED"
	ResponseCodeServerBug            ResponseCode = "SERVERBUG"
	ResponseCodeTryCreate            ResponseCode = "TRYCREATE"
	ResponseCodeUnavailable          ResponseCode = "UNAVAILABLE"
	ResponseCodeUnknownCTE           ResponseCode = "UNKNOWN-CTE"

	// METADATA
	ResponseCodeTooMany     ResponseCode = "TOOMANY"
	ResponseCodeNoPrivate   ResponseCode = "NOPRIVATE"
	ResponseCodeLongEntries ResponseCode = "LONGENTRIES"
	ResponseCodeMaxSize     ResponseCode = "MAXSIZE"

	// APPENDLIMIT
	ResponseCodeTooBig ResponseCode = "TOOBIG"

	// NOTIFY (RFC 5465)
	ResponseCodeNotificationOverflow ResponseCode = "NOTIFICATIONOVERFLOW"
	ResponseCodeBadEvent             ResponseCode = "BADEVENT"

	// CONDSTORE
	ResponseCodeModified ResponseCode = "MODIFIED"
)

// ModifiedResponseCode returns the MODIFIED response code (RFC 7162 §3.1.3)
// carrying the messages that a conditional STORE left untouched because they
// failed its UNCHANGEDSINCE precondition.
//
// numSet must be expressed in the number space of the command that produced it:
// UIDs for UID STORE, sequence numbers for STORE. It is returned complete with
// its argument (e.g. "MODIFIED 7,9"), ready to use as StatusResponse.Code, so
// that a server writes a tagged completion of the form:
//
//	A1 OK [MODIFIED 7,9] Conditional STORE failed for some messages
//
// A backend reaches that tagged line by returning an *Error from Session.Store:
//
//	return &imap.Error{
//		Type: imap.StatusResponseTypeOK, // or StatusResponseTypeNo, see below
//		Code: imap.ModifiedResponseCode(failed),
//		Text: "Conditional STORE failed for some messages",
//	}
//
// Use StatusResponseTypeOK when the listed messages merely failed the
// precondition, and StatusResponseTypeNo when they no longer exist because they
// were expunged (RFC 7162 §3.1.3 and §3.1.5).
//
// An empty or nil numSet yields the empty ResponseCode rather than a bare
// "[MODIFIED]", which the grammar does not allow: a conditional STORE in which
// every message satisfied the precondition must not report MODIFIED at all.
//
// The returned value carries its argument, so it never compares equal to
// ResponseCodeModified; on the client side, imapclient parses the bare code
// atom into Error.Code and exposes the set via FetchCommand.Modified. Match
// with strings.HasPrefix (or use Modified) rather than equality.
func ModifiedResponseCode(numSet NumSet) ResponseCode {
	if numSet == nil {
		return ""
	}
	s := numSet.String()
	if s == "" {
		return ""
	}
	return ResponseCode(fmt.Sprintf("%v %v", ResponseCodeModified, s))
}

// StatusResponse is a generic status response.
//
// See RFC 9051 section 7.1.
type StatusResponse struct {
	Type StatusResponseType
	Code ResponseCode
	Text string
}

// Error is an IMAP error caused by a status response.
type Error StatusResponse

var _ error = (*Error)(nil)

// Error implements the error interface.
func (err *Error) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "imap: %v", err.Type)
	if err.Code != "" {
		fmt.Fprintf(&sb, " [%v]", err.Code)
	}
	text := err.Text
	if text == "" {
		text = "<unknown>"
	}
	fmt.Fprintf(&sb, " %v", text)
	return sb.String()
}
