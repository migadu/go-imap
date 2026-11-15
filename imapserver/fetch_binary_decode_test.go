package imapserver_test

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	gomessage "github.com/emersion/go-message"
	"golang.org/x/text/encoding/charmap"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
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
	if resp := rc.do("FETCH 2 (BINARY.PEEK[1] BINARY.SIZE[1])"); !strings.Contains(resp, "BINARY[1] {5}\r\nplain BINARY.SIZE[1] 5)") {
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
		want: "BINARY[1] {14}\r\nline1\r\nline2\r\n BINARY.SIZE[1] 14)",
	}, {
		name: "base64 text with CRLF and LF",
		msg:  singlePart("text/html", "base64", "YQ0KYgo=\r\n"), // "a\r\nb\n"
		want: "BINARY[1] {6}\r\na\r\nb\r\n BINARY.SIZE[1] 6)",
	}, {
		name: "quoted-printable text with an encoded LF",
		msg:  singlePart("text/plain", "quoted-printable", "a=0Ab\r\n"),
		want: "BINARY[1] {6}\r\na\r\nb\r\n BINARY.SIZE[1] 6)",
	}, {
		name: "base64 non-text keeps its octets",
		msg:  singlePart("application/octet-stream", "base64", "bGluZTEKbGluZTIK\r\n"),
		want: "BINARY[1] {12}\r\nline1\nline2\n BINARY.SIZE[1] 12)",
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

	const want = "BINARY[1] {6}\r\ncaf\xe9\r\n BINARY.SIZE[1] 6)"
	if resp := rc.do("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1])"); !strings.Contains(resp, want) {
		t.Errorf("FETCH 1 (BINARY.PEEK[1] BINARY.SIZE[1]) → %q, want %q in it", resp, want)
	}
	// An unknown charset does not hide the unknown transfer encoding.
	if resp, tagged := rc.try("FETCH 2 (BINARY.PEEK[1])"); !strings.Contains(tagged, " NO [UNKNOWN-CTE] ") {
		t.Errorf("FETCH 2 (BINARY.PEEK[1]) → %q, want NO [UNKNOWN-CTE]", resp)
	}
}

// RFC 9051 §7.5.2, BINARY[<section-binary>]<<number>>: "If the domain of the
// decoded data is "8bit" and the data does not contain the NUL octet, the
// server SHOULD return the data in a <string> instead of a <literal8>; this
// allows the client to determine if the "8bit" data contains the NUL octet
// without having to explicitly scan the data stream for NULs." (RFC 3516 §4.3
// likewise.) A literal carries any octet but NUL (RFC 9051 §9, "CHAR8 =
// %x01-ff"), so the data sent decides: a <literal8> only when it holds a NUL.
func TestFetchBinaryLiteral8OnlyWithNUL(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}})
	rc.appendMessage(singlePart("application/octet-stream", "base64", "AAH/YQ==\r\n")) // "\x00\x01\xffa"
	text := singlePart("text/plain; charset=utf-8", "8bit", "caf\xc3\xa9\r\n")
	rc.appendMessage(text)
	rc.do("SELECT INBOX")

	for _, tc := range []struct{ cmd, want string }{
		{"FETCH 1 (BINARY.PEEK[1])", "BINARY[1] ~{4}\r\n\x00\x01\xffa)"},
		{"FETCH 1 (BINARY.PEEK[1]<0.2>)", "BINARY[1]<0> ~{2}\r\n\x00\x01)"},
		{"FETCH 1 (BINARY.PEEK[1]<1.3>)", "BINARY[1]<1> {3}\r\n\x01\xffa)"},
		{"FETCH 1 (BINARY.PEEK[1]<4.1>)", "BINARY[1]<4> {0}\r\n)"},
		{"FETCH 2 (BINARY.PEEK[1])", "BINARY[1] {7}\r\ncaf\xc3\xa9\r\n)"},
		{"FETCH 2 (BINARY.PEEK[])", "BINARY[] {" + strconv.Itoa(len(text)) + "}\r\n" + text + ")"},
	} {
		if resp := rc.do(tc.cmd); !strings.Contains(resp, tc.want) {
			t.Errorf("%s → %q, want %q in it", tc.cmd, resp, tc.want)
		}
	}
}

func TestFetchBinaryUnknownCTENoPartialResponseOrSeenMutation(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}})
	rc.appendMessage(singlePart("text/plain", "base64", "aGVsbG8=\r\n"))
	rc.appendMessage("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: x-uuencode\r\n\r\nbegin 644 a.txt\r\nend\r\n" +
		"--b--\r\n")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		"FETCH 2 (BINARY.PEEK[2])",
		"FETCH 2 (FLAGS BINARY.PEEK[2])",
		"FETCH 2 (BINARY.PEEK[1] BINARY.PEEK[2])",
		"FETCH 2 (BINARY.PEEK[1] BINARY.SIZE[2])",
	} {
		resp, tagged := rc.try(cmd)
		if !strings.Contains(tagged, " NO [UNKNOWN-CTE] ") {
			t.Fatalf("%s → tagged %q, want NO [UNKNOWN-CTE]", cmd, tagged)
		}
		if strings.Contains(resp, "* 2 FETCH") {
			t.Errorf("%s emitted partial untagged FETCH for failing message 2 before NO [UNKNOWN-CTE]: %q", cmd, resp)
		}
	}

	resp, tagged := rc.try("FETCH 1:2 (BODY[] BINARY[2])")
	if !strings.Contains(tagged, " NO [UNKNOWN-CTE] ") {
		t.Fatalf("FETCH 1:2 (BODY[] BINARY[2]) → tagged %q, want NO [UNKNOWN-CTE]", tagged)
	}
	if strings.Contains(resp, "* 2 FETCH") {
		t.Errorf("FETCH 1:2 (BODY[] BINARY[2]) emitted partial untagged FETCH for message 2: %q", resp)
	}
	rc.do("NOOP")
	flags2 := rc.do("FETCH 2 (FLAGS)")
	if strings.Contains(strings.ToLower(flags2), `\seen`) {
		t.Errorf("failed FETCH marked message 2 \\Seen: %q", flags2)
	}
}

func TestNotifyMessageNewBinaryUnknownCTEDoesNotAbortConn(t *testing.T) {
	user := imapmemserver.NewUser("user", "pass")
	memServer := imapmemserver.New()
	memServer.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapNotify:    {},
			imap.CapBinary:    {},
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	addr := ln.Addr().String()

	watcher := dialNotifyTest(t, addr)
	defer watcher.close()
	watcher.login(t)
	watcher.cmd("CREATE INBOX")
	watcher.cmd("SELECT INBOX")
	if resp := watcher.cmd("NOTIFY SET (SELECTED (MessageNew (UID BINARY.PEEK[1]) MessageExpunge))"); !strings.Contains(resp, "OK") {
		t.Fatalf("NOTIFY SET failed: %q", resp)
	}

	other := dialNotifyTest(t, addr)
	defer other.close()
	other.login(t)
	other.cmd("CREATE Staging")

	uuMsg := singlePart("text/plain", "x-uuencode", "begin 644 a.txt\r\n#86)C\r\n`\r\nend\r\n")
	b64Msg := singlePart("text/plain", "base64", "aGVsbG8gd29ybGQ=\r\n")
	for _, msg := range []string{uuMsg, b64Msg} {
		other.tag++
		tag := "A" + strconv.Itoa(other.tag)
		other.write(tag + " APPEND Staging {" + strconv.Itoa(len(msg)) + "}\r\n")
		cont, err := other.readLine(5 * time.Second)
		if err != nil || !strings.HasPrefix(cont, "+") {
			t.Fatalf("APPEND continuation: %v %q", err, cont)
		}
		other.write(msg + "\r\n")
		line, err := other.readLine(5 * time.Second)
		if err != nil || !strings.Contains(line, " OK ") {
			t.Fatalf("APPEND completion: %v %q", err, line)
		}
	}
	other.cmd("SELECT Staging")
	other.cmd("COPY 1:2 INBOX")

	var lines []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := watcher.readLine(300 * time.Millisecond)
		if err != nil {
			break
		}
		lines = append(lines, line)
		if strings.Contains(line, "hello world") {
			break
		}
	}
	noopResp := watcher.cmd("NOOP")
	all := strings.Join(lines, "") + noopResp
	if strings.Contains(all, "* 1 FETCH") {
		t.Errorf("watcher received partial FETCH for undecodable message 1: %q", all)
	}
	if !strings.Contains(all, "* 2 FETCH") || !strings.Contains(all, "hello world") {
		t.Errorf("watcher missed MessageNew FETCH for valid message 2: %q", all)
	}
}
