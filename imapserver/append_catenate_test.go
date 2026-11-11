package imapserver_test

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type bytesLiteral struct{ *bytes.Reader }

func (l bytesLiteral) Size() int64 { return l.Reader.Size() }

// catenateSession adds CATENATE to the in-memory session. It resolves URLs
// from a fixed table, which is all the server side of the extension needs:
// reading IMAP URLs is the backend's part.
type catenateSession struct {
	imapserver.Session
	urls  map[string]string
	limit uint32
}

func (s *catenateSession) AppendLimit() uint32 { return s.limit }

func (s *catenateSession) AppendCatenate(ctx context.Context, mailbox string, parts imapserver.CatenateReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	if mailbox == "Missing" {
		// Fails without reading a single part.
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	var msg bytes.Buffer
	for {
		part, err := parts.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if part.Text != nil {
			if _, err := io.Copy(&msg, part.Text); err != nil {
				return nil, err
			}
			continue
		}
		data, ok := s.urls[part.URL]
		if !ok {
			return nil, &imapserver.BadURLError{URL: part.URL}
		}
		msg.WriteString(data)
	}
	return s.Session.Append(ctx, mailbox, bytesLiteral{bytes.NewReader(msg.Bytes())}, options)
}

func newCatenateConn(t *testing.T, limit uint32) *searchRev2Conn {
	t.Helper()
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCatenate: {}, imap.CapLiteralPlus: {}}
	return newSearchRev2ConnWrapped(t, caps, func(sess imapserver.Session) imapserver.Session {
		return &catenateSession{
			Session: sess,
			urls: map[string]string{
				"/INBOX/;UID=1/;SECTION=HEADER": "Subject: cat\r\n\r\n",
				"/INBOX/;UID=1/;SECTION=1":      "[part one]",
				"u":                             "",
			},
			limit: limit,
		}
	})
}

// exchange sends a command in chunks. Each chunk but the last ends in a
// synchronizing literal announcement, for which the server must answer with a
// continuation request before the next chunk goes out. If the server answers
// with the tagged response instead, exchange stops there. It returns the tagged
// line and how many continuation requests it saw.
func (rc *searchRev2Conn) exchange(chunks ...string) (tagged string, contReqs int) {
	rc.t.Helper()
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	for i, chunk := range chunks {
		if i == 0 {
			chunk = tag + " " + chunk
		}
		if _, err := rc.c.Write([]byte(chunk + "\r\n")); err != nil {
			rc.t.Fatalf("write %q: %v", chunk, err)
		}
		for {
			line, err := rc.br.ReadString('\n')
			if err != nil {
				rc.t.Fatalf("reading after %q: %v", chunk, err)
			}
			if strings.HasPrefix(line, tag+" ") {
				return line, contReqs
			}
			if strings.HasPrefix(line, "+") {
				contReqs++
				break
			}
		}
	}
	rc.t.Fatalf("the server asked for more than the %d chunks of the command", len(chunks))
	return "", 0
}

// requireInSync fails the test unless the server reads the next command from
// a clean position.
func (rc *searchRev2Conn) requireInSync(after string) {
	rc.t.Helper()
	if _, tagged := rc.try("NOOP"); !strings.Contains(tagged, " OK ") {
		rc.t.Errorf("after %s: NOOP → %q, want OK", after, tagged)
	}
}

// RFC 4469 §2: "A server that supports this extension returns "CATENATE" as
// one of the responses to the CAPABILITY command." §3: "If the extended form
// is used, "CATENATE" and a parenthesized list of message literals and message
// URLs follows, each of which is appended to the new message."
//
//	append-data =/ "CATENATE" SP "(" cat-part *(SP cat-part) ")"
//	cat-part = text-literal / url
//	text-literal = "TEXT" SP literal
//	url = "URL" SP astring
//
// Upstream request: emersion/go-imap#315.
func TestAppendCatenate(t *testing.T) {
	rc := newCatenateConn(t, 1<<20)
	if resp := rc.do("CAPABILITY"); !strings.Contains(resp, " CATENATE") {
		t.Errorf("CAPABILITY does not offer CATENATE: %q", resp)
	}

	// A synchronizing literal, then a non-synchronizing one, around URLs.
	tagged, contReqs := rc.exchange(
		`APPEND INBOX (\Seen) CATENATE (URL "/INBOX/;UID=1/;SECTION=HEADER" TEXT {7}`,
		"hello, "+` URL "/INBOX/;UID=1/;SECTION=1" TEXT {3+}`+"\r\n!\r\n)",
	)
	if !strings.Contains(tagged, " OK ") {
		t.Fatalf("APPEND CATENATE → %q, want OK", tagged)
	}
	if contReqs != 1 {
		t.Errorf("got %d continuation requests, want 1 (one synchronizing literal)", contReqs)
	}

	rc.do("SELECT INBOX")
	resp := rc.do("FETCH 1 (FLAGS BODY.PEEK[])")
	if want := "Subject: cat\r\n\r\nhello, [part one]!\r\n"; !strings.Contains(resp, want) {
		t.Errorf("the appended message is %q, want it to hold %q", resp, want)
	}
	if !strings.Contains(strings.ToLower(resp), `\seen`) {
		t.Errorf("the appended message lacks the flag of the command: %q", resp)
	}
}

// RFC 4469 §4.1: "The BADURL response code is returned if the APPEND fails to
// process one of the specified URLs. [...] The BADURL response code contains
// the first URL specified as a parameter to the APPEND command that has caused
// the operation to fail."
//
// The command ends there, but what the client sent of it is still read: a
// non-synchronizing literal is on its way whatever the server answers.
func TestAppendCatenateBadURL(t *testing.T) {
	rc := newCatenateConn(t, 1<<20)

	_, tagged := rc.try(`APPEND INBOX CATENATE (URL "/INBOX/;UID=99" TEXT {3+}` + "\r\nabc)")
	if !strings.Contains(tagged, " NO [BADURL /INBOX/;UID=99] ") {
		t.Errorf("APPEND with a bad URL → %q, want NO [BADURL /INBOX/;UID=99]", tagged)
	}
	rc.requireInSync("a bad URL ahead of a non-synchronizing literal")

	// No continuation request for a literal of a command that has failed.
	tagged, contReqs := rc.exchange(`APPEND INBOX CATENATE (URL "/INBOX/;UID=99" TEXT {3}`, "abc)")
	if !strings.Contains(tagged, " NO [BADURL /INBOX/;UID=99] ") || contReqs != 0 {
		t.Errorf("APPEND with a bad URL → %q after %d continuation requests, want NO [BADURL ...] after none", tagged, contReqs)
	}
	rc.requireInSync("a bad URL ahead of a synchronizing literal")

	// Nor for a URL sent as one.
	tagged, contReqs = rc.exchange(`APPEND INBOX CATENATE (URL "/INBOX/;UID=99" URL {1}`, "u)")
	if !strings.Contains(tagged, " NO [BADURL /INBOX/;UID=99] ") || contReqs != 0 {
		t.Errorf("APPEND with a bad URL → %q after %d continuation requests, want NO [BADURL ...] after none", tagged, contReqs)
	}
	rc.requireInSync("a bad URL ahead of a URL in a synchronizing literal")

	// The literal the server did ask for is read to its end, and the one
	// after it too, before the next command.
	tagged, contReqs = rc.exchange("APPEND INBOX CATENATE (TEXT {3}", `abc URL "/INBOX/;UID=99" TEXT {3+}`+"\r\ndef)")
	if !strings.Contains(tagged, " NO [BADURL /INBOX/;UID=99] ") || contReqs != 1 {
		t.Errorf("APPEND with a bad URL → %q after %d continuation requests, want NO [BADURL ...] after one", tagged, contReqs)
	}
	rc.requireInSync("a bad URL between two literals")

	// url-resp-text is "Any TEXT-CHAR except ]" (§5): a URL that cannot be
	// written in the response code is refused without it.
	_, tagged = rc.try(`APPEND INBOX CATENATE (URL "/INBOX]/;UID=99")`)
	if !strings.Contains(tagged, " NO ") || strings.Contains(tagged, "]/;UID") {
		t.Errorf("APPEND with a URL holding \"]\" → %q, want NO without the URL", tagged)
	}
	rc.requireInSync("a bad URL that has no response-code form")

	// A session error that is not about a URL: the parts are still read.
	_, tagged = rc.try("APPEND Missing CATENATE (TEXT {3+}\r\nabc TEXT {3+}\r\ndef)")
	if !strings.Contains(tagged, " NO [TRYCREATE] ") {
		t.Errorf("APPEND to a missing mailbox → %q, want NO [TRYCREATE]", tagged)
	}
	rc.requireInSync("a session error ahead of the parts")
}

// RFC 4469 §4.2: "The server may also wish to return the TOOBIG response code
// if the resulting message exceeds a server-specific message size limit." The
// TEXT parts together are held to the APPEND limit.
func TestAppendCatenateTooBig(t *testing.T) {
	rc := newCatenateConn(t, 10)

	_, tagged := rc.try("APPEND INBOX CATENATE (TEXT {6+}\r\nabcdef TEXT {6+}\r\nabcdef)")
	if !strings.Contains(tagged, " NO [TOOBIG] ") {
		t.Errorf("APPEND over the limit → %q, want NO [TOOBIG]", tagged)
	}
	rc.requireInSync("TEXT parts over the limit, non-synchronizing")

	tagged, contReqs := rc.exchange("APPEND INBOX CATENATE (TEXT {6}", "abcdef TEXT {6}", "abcdef)")
	if !strings.Contains(tagged, " NO [TOOBIG] ") || contReqs != 1 {
		t.Errorf("APPEND over the limit → %q after %d continuation requests, want NO [TOOBIG] after one", tagged, contReqs)
	}
	rc.requireInSync("TEXT parts over the limit, synchronizing")

	_, tagged = rc.try("APPEND INBOX CATENATE (TEXT {5+}\r\nabcde TEXT {5+}\r\nabcde)")
	if !strings.Contains(tagged, " OK ") {
		t.Errorf("APPEND at the limit → %q, want OK", tagged)
	}
}

// Every URL part is a lookup for the session, so their number is bounded
// (RFC 9051 §7.1, LIMIT: "The operation ran up against an implementation
// limit of some kind").
func TestAppendCatenateTooManyParts(t *testing.T) {
	rc := newCatenateConn(t, 1<<20)

	_, tagged := rc.try("APPEND INBOX CATENATE (" + strings.TrimSuffix(strings.Repeat("URL u ", 1001), " ") + ")")
	if !strings.Contains(tagged, " NO [LIMIT] ") {
		t.Errorf("APPEND of 1001 parts → %q, want NO [LIMIT]", tagged)
	}
	rc.requireInSync("too many parts")

	_, tagged = rc.try("APPEND INBOX CATENATE (TEXT {17+}\r\nSubject: many\r\n\r\n " + strings.TrimSuffix(strings.Repeat("URL u ", 999), " ") + ")")
	if !strings.Contains(tagged, " OK ") {
		t.Errorf("APPEND of 1000 parts → %q, want OK", tagged)
	}
}

func TestAppendCatenateSyntax(t *testing.T) {
	rc := newCatenateConn(t, 1<<20)
	for _, args := range []string{
		"CATENATE",
		"CATENATE ()",
		`CATENATE (FOO "x")`,
		`CATENATE (URL)`,
		`CATENATE (URL "/INBOX/;UID=1/;SECTION=1"`,
		`CATENATE (URL "/INBOX/;UID=1/;SECTION=1" )`,
		`CATENATE (URL "/INBOX/;UID=1/;SECTION=1") x`,
		`CATENATE (TEXT "abc")`,
	} {
		if _, tagged := rc.try("APPEND INBOX " + args); !strings.Contains(tagged, " BAD ") {
			t.Errorf("APPEND INBOX %s → %q, want BAD", args, tagged)
		}
		rc.requireInSync("APPEND INBOX " + args)
	}
	rc.do("SELECT INBOX")
	if resp := rc.do("STATUS INBOX (MESSAGES)"); !strings.Contains(resp, "MESSAGES 0") {
		t.Errorf("a refused APPEND stored a message: %q", resp)
	}
}

// Without a session that implements it, CATENATE is neither offered nor
// read, whatever the configured capabilities say.
func TestAppendCatenateNotOffered(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCatenate: {}})
	if resp := rc.do("CAPABILITY"); strings.Contains(resp, "CATENATE") {
		t.Errorf("CAPABILITY offers CATENATE without a session for it: %q", resp)
	}
	if _, tagged := rc.try(`APPEND INBOX CATENATE (URL "/INBOX/;UID=1")`); !strings.Contains(tagged, " BAD ") {
		t.Errorf("APPEND CATENATE → %q, want BAD", tagged)
	}
}
