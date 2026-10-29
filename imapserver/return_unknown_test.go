package imapserver_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// sendRaw runs one command on rc without requiring OK and returns the whole
// response and its tagged line.
func sendRaw(rc *searchRev2Conn, cmd string) (resp, tagged string) {
	rc.t.Helper()
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	if _, err := rc.c.Write([]byte(tag + " " + cmd + "\r\n")); err != nil {
		rc.t.Fatalf("write %q: %v", cmd, err)
	}
	var buf bytes.Buffer
	for {
		line, err := rc.br.ReadString('\n')
		buf.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			return buf.String(), line
		}
		if err != nil {
			rc.t.Fatalf("reading response to %q: %v (got %q)", cmd, err, buf.String())
		}
	}
}

// RFC 4466 §2.6.1: "Any options not defined by extensions that the server
// supports must be rejected with a BAD response." RFC 5267 §3 makes the
// extended SORT behave as the extended SEARCH. Unknown options used to be
// ignored, which turned RETURN (FOO) into an ALL the client never asked for.
func TestUnknownReturnOptionRefused(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {},
		imap.CapSort: {}, imap.CapESort: {}, imap.CapMultiSearch: {},
	})
	rc.appendOne("one")
	rc.appendOne("two")
	rc.do("SELECT INBOX")

	for _, cmd := range []string{
		"SEARCH RETURN (FOO) ALL",
		"UID SEARCH RETURN (MIN PARTIAL 1:10) ALL",
		`SEARCH RETURN (COUNT FOO (BAR "baz")) ALL`,
		"ESEARCH IN (MAILBOXES INBOX) RETURN (FOO) ALL",
		"SORT RETURN (FOO) (ARRIVAL) UTF-8 ALL",
		"SORT RETURN (SAVE) (ARRIVAL) UTF-8 ALL",
		"UID SORT RETURN (ALL CONTEXT) (ARRIVAL) UTF-8 ALL",
	} {
		if resp, tagged := sendRaw(rc, cmd); !strings.Contains(tagged, " BAD [CLIENTBUG] Unknown ") {
			t.Errorf("%s: want BAD [CLIENTBUG] Unknown … return option, got:\n%s", cmd, resp)
		}
		// The rest of the refused line, parameters included, is discarded.
		rc.do("NOOP")
	}

	// The options this server supports still work.
	for cmd, want := range map[string]string{
		"SEARCH RETURN (MIN MAX COUNT ALL SAVE) ALL":      " MIN 1 MAX 2 COUNT 2\r\n",
		"SEARCH RETURN () ALL":                            " ALL 1:2\r\n",
		"ESEARCH IN (MAILBOXES INBOX) RETURN (COUNT) ALL": " COUNT 2\r\n",
		"SORT RETURN (MIN MAX COUNT) (ARRIVAL) UTF-8 ALL": " COUNT 2\r\n",
		"SORT RETURN () (ARRIVAL) UTF-8 ALL":              " ALL 1,2\r\n", // in sort order
	} {
		if resp := rc.do(cmd); !strings.Contains(resp, want) {
			t.Errorf("%s: want %q, got:\n%s", cmd, strings.TrimSpace(want), resp)
		}
	}
}
