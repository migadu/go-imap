package imapserver_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

func multiSaveCaps() imap.CapSet {
	return imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {}, imap.Cap("MULTISEARCH"): {}}
}

// RFC 7377 section 2.2 permits SAVE only for the selected source. RFC 5182
// requires SAVE alone to suppress results while retaining the matches for $.
func TestMultiSearchSaveSelected(t *testing.T) {
	for _, source := range []string{"", "IN (selected) "} {
		t.Run(source, func(t *testing.T) {
			rc := newSearchRev2Conn(t, multiSaveCaps())
			rc.appendOne("one")
			rc.appendOne("two")
			rc.appendOne("three")
			rc.do("SELECT INBOX")

			for _, tc := range []struct {
				options, criteria, result, saved string
			}{
				{"SAVE", "UID 2:3", "", "2:3"},
				{"SAVE COUNT", "UID 1:2", " COUNT 2", "1:2"},
				{"SAVE ALL", "UID 2:3", " ALL 2:3", "2:3"},
				{"SAVE", "UID 99", "", ""},
				{"SAVE COUNT", "UID 99", "", ""},
			} {
				resp, tagged := rc.try("ESEARCH " + source + "RETURN (" + tc.options + ") " + tc.criteria)
				if !strings.Contains(tagged, " OK ") {
					t.Fatalf("ESEARCH: %q", resp)
				}
				if tc.result == "" {
					if resp != tagged {
						t.Errorf("SAVE-only emitted results: %q", resp)
					}
				} else if !strings.Contains(resp, tc.result) || !strings.Contains(resp, "MAILBOX INBOX UIDVALIDITY ") {
					t.Errorf("mixed SAVE response = %q, want %q with mailbox correlator", resp, tc.result)
				}
				check := rc.do("UID SEARCH RETURN (ALL) $")
				if tc.saved == "" {
					if strings.Contains(check, " ALL ") {
						t.Errorf("empty SAVE did not clear $: %q", check)
					}
				} else if !strings.Contains(check, " ALL "+tc.saved+"\r\n") {
					t.Errorf("saved result = %q, want %s", check, tc.saved)
				}
				// Reading $ through MULTISEARCH must use the same selected
				// session variable even when this command does not save it.
				check, tagged = rc.try("ESEARCH IN (selected) RETURN (ALL) $")
				if !strings.Contains(tagged, " OK ") ||
					(tc.saved == "" && check != tagged) ||
					(tc.saved != "" && !strings.Contains(check, " ALL "+tc.saved+"\r\n")) {
					t.Errorf("MULTISEARCH $ = %q, want saved UIDs %q", check, tc.saved)
				}
			}
		})
	}
}

type multiSaveSession struct {
	imapserver.Session
	caps  imap.CapSet
	calls *atomic.Int32
	fail  bool
}

func (s *multiSaveSession) GetCapabilities() imap.CapSet { return s.caps }

func (s *multiSaveSession) MultiSearch(ctx context.Context, source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	s.calls.Add(1)
	if s.fail {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Cannot save results"}
	}
	return s.Session.(imapserver.SessionMultiSearch).MultiSearch(ctx, source, criteria, options)
}

func TestMultiSearchSaveValidation(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		searchRes    bool
		fail         bool
		wantStatus   string
		wantCalls    int32
	}{
		{name: "missing SEARCHRES", source: "selected", wantStatus: "BAD"},
		{name: "personal source", source: "personal", searchRes: true, wantStatus: "BAD"},
		{name: "mixed sources", source: "selected personal", searchRes: true, wantStatus: "BAD"},
		{name: "named selected mailbox", source: "mailboxes INBOX", searchRes: true, wantStatus: "BAD"},
		{name: "backend failure", source: "selected", searchRes: true, fail: true, wantStatus: "NO", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			caps := multiSaveCaps()
			if !tc.searchRes {
				delete(caps, imap.CapSearchRes)
			}
			rc := newSearchRev2ConnWrapped(t, multiSaveCaps(), func(s imapserver.Session) imapserver.Session {
				return &multiSaveSession{Session: s, caps: caps, calls: &calls, fail: tc.fail}
			})
			rc.appendOne("one")
			rc.do("SELECT INBOX")
			resp, tagged := rc.try("ESEARCH IN (" + tc.source + ") RETURN (SAVE) ALL")
			if !strings.Contains(tagged, " "+tc.wantStatus+" ") || resp != tagged {
				t.Errorf("ESEARCH response = %q, want only tagged %s", resp, tc.wantStatus)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("MultiSearch calls = %d, want %d", got, tc.wantCalls)
			}
			rc.do("NOOP")
		})
	}
}
