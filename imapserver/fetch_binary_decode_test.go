package imapserver_test

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	gomessage "github.com/emersion/go-message"
	"golang.org/x/text/encoding/charmap"

	"github.com/migadu/go-imap"
)

// appendMessage stores msg in INBOX through a synchronizing literal.
func (rc *searchRev2Conn) appendMessage(msg string) {
	rc.t.Helper()
	tagged, _ := rc.exchange("APPEND INBOX {"+strconv.Itoa(len(msg))+"}", msg)
	if !strings.Contains(tagged, " OK ") {
		rc.t.Fatalf("APPEND → %s", strings.TrimRight(tagged, "\r\n"))
	}
}

func singlePart(contentType, cte, body string) string {
	return "From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: binary\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: " + contentType + "\r\n" +
		"Content-Transfer-Encoding: " + cte + "\r\n" +
		"\r\n" +
		body
}

// RFC 9051 §7.5.2, BINARY[<section-binary>]<<number>>: "If the server does
// not know how to decode the section's Content-Transfer-Encoding, it MUST fail
// the request and issue a "NO" response that contains the "UNKNOWN-CTE"
// response code." BINARY.SIZE[<section-binary>] says the same, as does
// RFC 3516 §4.3.
func TestFetchBinaryUnknownCTE(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}})
	rc.appendMessage(singlePart("text/plain", "x-uuencode", "begin 644 a.txt\r\n#86)C\r\n`\r\nend\r\n"))
	rc.appendMessage("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: X-Weird\r\n\r\nencoded\r\n" +
		"--b--\r\n")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		"FETCH 1 (BINARY.PEEK[1])",
		"FETCH 1 (BINARY[1])",
		"FETCH 1 (BINARY.SIZE[1])",
		"FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1])",
		"FETCH 1 (BINARY.PEEK[1]<0.4>)",
		"FETCH 2 (BINARY.PEEK[2])",
		"FETCH 2 (BINARY.SIZE[2])",
		"FETCH 1:2 (BINARY.SIZE[1])",
	} {
		resp, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " NO [UNKNOWN-CTE] ") {
			t.Errorf("%s → %q, want NO [UNKNOWN-CTE]", cmd, resp)
		}
	}

	// The other part of the same message decodes. (The CRLF before a boundary
	// belongs to the boundary, RFC 2046 §5.1.1.)
	if resp := rc.do("FETCH 2 (BINARY.PEEK[1] BINARY.SIZE[1])"); !strings.Contains(resp, "BINARY[1] ~{5}\r\nplain BINARY.SIZE[1] 5)") {
		t.Errorf("FETCH 2 (BINARY.PEEK[1] BINARY.SIZE[1]) → %q", resp)
	}
}

// RFC 9051 §7.5.2: "When sending data in a BINARY[...] FETCH data item,
// servers MUST ensure that textual line-oriented sections are always
// transmitted using the IMAP CRLF line termination syntax, regardless of the
// underlying storage representation of the data on the server." The
// parenthesis before it names them: "textual data (text/* media types)".
// BINARY.SIZE "MUST match the size of the <nstring> or <literal8> that will be
// returned by the corresponding FETCH BINARY request".
func TestFetchBinaryTextUsesCRLF(t *testing.T) {
	for _, tc := range []struct {
		name, msg, want string
	}{{
		name: "base64 text with LF",
		msg:  singlePart("text/plain", "base64", "bGluZTEKbGluZTIK\r\n"), // "line1\nline2\n"
		want: "BINARY[1] ~{14}\r\nline1\r\nline2\r\n BINARY.SIZE[1] 14)",
	}, {
		name: "base64 text with CRLF and LF",
		msg:  singlePart("text/html", "base64", "YQ0KYgo=\r\n"), // "a\r\nb\n"
		want: "BINARY[1] ~{6}\r\na\r\nb\r\n BINARY.SIZE[1] 6)",
	}, {
		name: "quoted-printable text with an encoded LF",
		msg:  singlePart("text/plain", "quoted-printable", "a=0Ab\r\n"),
		want: "BINARY[1] ~{6}\r\na\r\nb\r\n BINARY.SIZE[1] 6)",
	}, {
		name: "base64 non-text keeps its octets",
		msg:  singlePart("application/octet-stream", "base64", "bGluZTEKbGluZTIK\r\n"),
		want: "BINARY[1] ~{12}\r\nline1\nline2\n BINARY.SIZE[1] 12)",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}})
			rc.appendMessage(tc.msg)
			rc.do("SELECT INBOX")
			if resp := rc.do("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1])"); !strings.Contains(resp, tc.want) {
				t.Errorf("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1]) → %q, want %q in it", resp, tc.want)
			}
		})
	}
}

// RFC 9051 §6.4.5: BINARY "Requests that the specified section be transmitted
// after performing decoding of the section's Content-Transfer-Encoding." The
// charset is not part of that: the octets stay in the charset the part's
// Content-Type names, even on a server that installs a go-message
// CharsetReader to read messages in other charsets.
func TestFetchBinaryKeepsCharset(t *testing.T) {
	old := gomessage.CharsetReader
	gomessage.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "iso-8859-1") {
			return charmap.ISO8859_1.NewDecoder().Reader(input), nil
		}
		return nil, fmt.Errorf("unhandled charset %q", charset)
	}
	t.Cleanup(func() { gomessage.CharsetReader = old })

	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}})
	rc.appendMessage(singlePart("text/plain; charset=iso-8859-1", "quoted-printable", "caf=E9\r\n"))
	rc.appendMessage(singlePart("text/plain; charset=x-unknown", "x-uuencode", "begin 644 a.txt\r\n#86)C\r\n`\r\nend\r\n"))
	rc.do("SELECT INBOX")

	const want = "BINARY[1] ~{6}\r\ncaf\xe9\r\n BINARY.SIZE[1] 6)"
	if resp := rc.do("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1])"); !strings.Contains(resp, want) {
		t.Errorf("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1]) → %q, want %q in it", resp, want)
	}
	// An unknown charset does not hide the unknown transfer encoding.
	if resp, tagged := rc.try("FETCH 2 (BINARY.PEEK[1])"); !strings.Contains(tagged, " NO [UNKNOWN-CTE] ") {
		t.Errorf("FETCH 2 (BINARY.PEEK[1]) → %q, want NO [UNKNOWN-CTE]", resp)
	}
}
