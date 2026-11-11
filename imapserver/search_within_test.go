package imapserver_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// appendDated stores one trivial message in INBOX with the given INTERNALDATE.
func (rc *searchRev2Conn) appendDated(dateTime string) {
	rc.t.Helper()
	body := "Subject: dated\r\n\r\nbody\r\n"
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	cmd := tag + " APPEND INBOX \"" + dateTime + "\" {" + strconv.Itoa(len(body)) + "+}\r\n" + body + "\r\n"
	if _, err := rc.c.Write([]byte(cmd)); err != nil {
		rc.t.Fatalf("APPEND: %v", err)
	}
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			rc.t.Fatalf("APPEND completion: %v", err)
		}
		if strings.HasPrefix(line, tag+" ") {
			if !strings.Contains(line, " OK ") {
				rc.t.Fatalf("APPEND → %s", strings.TrimRight(line, "\r\n"))
			}
			return
		}
	}
}

// RFC 5032 §2: a server that supports the extension "MUST return WITHIN as
// one of the server supported capabilities", and for the OLDER and YOUNGER
// search keys it "calculates a target date and time by subtracting the
// interval, specified in seconds, from the current date and time of the
// server. [...] For OLDER, messages match if the INTERNALDATE is less recent
// than or equal to the target time. For YOUNGER, messages match if the
// INTERNALDATE is more recent than or equal to the target time."
//
// Upstream request: emersion/go-imap#373.
func TestSearchWithin(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapWithin: {}})
	if resp := rc.do("CAPABILITY"); !strings.Contains(resp, " WITHIN") {
		t.Errorf("CAPABILITY does not offer WITHIN: %q", resp)
	}

	rc.appendDated("01-Jan-2020 00:00:00 +0000") // UID 1, years old
	rc.appendOne("fresh")                        // UID 2, stored just now
	rc.do("SELECT INBOX")

	for _, tc := range []struct {
		criteria, want string
	}{
		{"YOUNGER 3600", " 2"},
		{"OLDER 3600", " 1"},
		{"younger 3600", " 2"},
		// The interval is the tighter of the two for YOUNGER, the wider
		// for OLDER: the keys intersect (RFC 9051 §6.4.4).
		{"YOUNGER 4000000000 YOUNGER 3600", " 2"},
		{"YOUNGER 3600 YOUNGER 4000000000", " 2"},
		{"OLDER 1 OLDER 3600", " 1"},
		{"OLDER 3600 OLDER 1", " 1"},
		{"YOUNGER 4000000000", " 1 2"},
		{"YOUNGER 3600 OLDER 3600", ""},
		{"NOT YOUNGER 3600", " 1"},
		{"OR OLDER 3600 YOUNGER 3600", " 1 2"},
		{"(UNSEEN YOUNGER 3600)", " 2"},
	} {
		resp := rc.do("UID SEARCH " + tc.criteria)
		if want := "* SEARCH" + tc.want + "\r\n"; !strings.HasPrefix(resp, want) {
			t.Errorf("UID SEARCH %s: got %q, want %q", tc.criteria, resp, want)
		}
	}

	// RFC 5032 §3: search-key =/ ( "OLDER" / "YOUNGER" ) SP nz-number
	for _, criteria := range []string{"YOUNGER 0", "OLDER 0", "YOUNGER", "OLDER x", "YOUNGER 4294967296"} {
		if _, tagged := rc.try("UID SEARCH " + criteria); !strings.Contains(tagged, " BAD ") {
			t.Errorf("UID SEARCH %s → %q, want BAD", criteria, tagged)
		}
	}
}

// A backend that was not configured for WITHIN is never handed the interval
// keys: it would not know to evaluate them, and the search would return
// messages they exclude.
func TestSearchWithinNotOffered(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	if resp := rc.do("CAPABILITY"); strings.Contains(resp, "WITHIN") {
		t.Errorf("CAPABILITY offers WITHIN without it being configured: %q", resp)
	}
	rc.appendOne("fresh")
	rc.do("SELECT INBOX")
	for _, criteria := range []string{"YOUNGER 3600", "OLDER 3600"} {
		if _, tagged := rc.try("UID SEARCH " + criteria); !strings.Contains(tagged, " BAD ") {
			t.Errorf("UID SEARCH %s → %q, want BAD", criteria, tagged)
		}
	}
}
