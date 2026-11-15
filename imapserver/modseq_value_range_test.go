package imapserver_test

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// CHANGEDSINCE and the QRESYNC select parameter take a mod-sequence-value
// (RFC 7162 §7):
//
//	chgsince-fetch-mod  = "CHANGEDSINCE" SP mod-sequence-value
//	mod-sequence-value  = 1*DIGIT
//	                       ;; Positive unsigned 63-bit integer
//	                       ;; (mod-sequence)
//	                       ;; (1 <= n <= 9,223,372,036,854,775,807).
//	mod-sequence-valzer = "0" / mod-sequence-value
//
// so 0, and anything past 2^63-1, is a syntax error there. 0 used to be read
// as if CHANGEDSINCE were absent: FETCH served every message, and with
// VANISHED the BAD said the client sent no CHANGEDSINCE.
func TestModSequenceValueRange(t *testing.T) {
	const (
		maxModSeq  = "9223372036854775807"
		overModSeq = "9223372036854775808"
	)
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapCondStore: {},
		imap.CapQResync:   {},
	})
	rc.appendOne("mod-sequence range")
	rc.do("ENABLE QRESYNC")

	for _, tc := range []struct {
		cmd string
		ok  bool
	}{
		{`SELECT INBOX (QRESYNC (1 0))`, false},
		{`SELECT INBOX (QRESYNC (1 0 1:10))`, false},
		{`SELECT INBOX (QRESYNC (1 ` + overModSeq + `))`, false},
		{`SELECT INBOX (QRESYNC (1 ` + maxModSeq + `))`, true},
		{`SELECT INBOX (QRESYNC (1 1))`, true},
	} {
		_, tagged := rc.try(tc.cmd)
		checkModSeqValueResponse(t, tc.cmd, tagged, tc.ok)
		rc.do("NOOP")
	}

	rc.do("SELECT INBOX")
	for _, tc := range []struct {
		cmd string
		ok  bool
	}{
		{`FETCH 1 (FLAGS) (CHANGEDSINCE 0)`, false},
		{`UID FETCH 1:* (FLAGS) (CHANGEDSINCE 0)`, false},
		{`UID FETCH 1:* (FLAGS) (CHANGEDSINCE 0 VANISHED)`, false},
		{`UID FETCH 1:* (FLAGS) (CHANGEDSINCE 0) VANISHED`, false},
		{`FETCH 1 FLAGS CHANGEDSINCE 0`, false},
		{`UID FETCH 1:* (FLAGS) (CHANGEDSINCE ` + overModSeq + `)`, false},
		{`UID FETCH 1:* (FLAGS) (CHANGEDSINCE ` + maxModSeq + ` VANISHED)`, true},
		{`UID FETCH 1:* (FLAGS) (CHANGEDSINCE 1)`, true},
	} {
		resp, tagged := rc.try(tc.cmd)
		checkModSeqValueResponse(t, tc.cmd, tagged, tc.ok)
		if !tc.ok && strings.Contains(resp, " FETCH (") {
			t.Errorf("%s: refused, but sent FETCH data:\n%s", tc.cmd, resp)
		}
		rc.do("NOOP")
	}

	// UNCHANGEDSINCE and the MODSEQ search key take a mod-sequence-valzer,
	// "0" / mod-sequence-value: 0 is allowed (RFC 7162 §3.1.3 gives
	// UNCHANGEDSINCE 0 a meaning), but the upper bound is the same 2^63-1.
	for _, tc := range []struct {
		cmd string
		ok  bool
	}{
		{`STORE 1 (UNCHANGEDSINCE 0) +FLAGS.SILENT (\Flagged)`, true},
		{`STORE 1 (UNCHANGEDSINCE ` + maxModSeq + `) +FLAGS.SILENT (\Flagged)`, true},
		{`STORE 1 (UNCHANGEDSINCE ` + overModSeq + `) +FLAGS.SILENT (\Flagged)`, false},
		{`UID STORE 1 (UNCHANGEDSINCE 18446744073709551615) +FLAGS.SILENT (\Flagged)`, false},
		{`SEARCH MODSEQ 0`, true},
		{`SEARCH MODSEQ ` + maxModSeq, true},
		{`SEARCH MODSEQ ` + overModSeq, false},
		{`UID SEARCH MODSEQ "/flags/\\seen" all ` + overModSeq, false},
	} {
		_, tagged := rc.try(tc.cmd)
		checkModSeqValueResponse(t, tc.cmd, tagged, tc.ok)
		rc.do("NOOP")
	}
}

func checkModSeqValueResponse(t *testing.T, cmd, tagged string, ok bool) {
	t.Helper()
	tagged = strings.TrimRight(tagged, "\r\n")
	switch {
	case ok && !strings.Contains(tagged, " OK "):
		t.Errorf("%s: want OK, got %q", cmd, tagged)
	case !ok && !strings.Contains(tagged, " BAD "):
		t.Errorf("%s: want BAD, got %q", cmd, tagged)
	case !ok && strings.Contains(tagged, "requires CHANGEDSINCE"):
		t.Errorf("%s: CHANGEDSINCE was sent, got %q", cmd, tagged)
	}
}
