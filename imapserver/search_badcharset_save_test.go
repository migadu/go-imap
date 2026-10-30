package imapserver_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// RFC 5182 §2.1 (RFC 9051 §6.4.4.1): "A SEARCH command with the SAVE result
// option that caused the server to return the NO tagged response sets the
// value of the search result variable to the empty sequence", while one
// answered BAD, or answered NO without SAVE, "MUST NOT change" it. RFC 9051
// §6.4.4.4 Example 5 is the unsupported-CHARSET case.
func TestSearchSaveBadCharsetEmptiesSearchRes(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		save bool
	}{
		{"UID SEARCH RETURN (SAVE) CHARSET KOI8-R UID 2", true},
		{"SEARCH RETURN (SAVE) CHARSET KOI8-R UID 2", true},
		{"UID SEARCH RETURN (COUNT SAVE) CHARSET KOI8-R UID 2", true},
		{"ESEARCH RETURN (SAVE) CHARSET KOI8-R UID 2", true},
		{"ESEARCH IN (selected) RETURN (SAVE) CHARSET KOI8-R UID 2", true},
		{"UID SEARCH CHARSET KOI8-R UID 2", false},
		{"UID SEARCH RETURN (ALL) CHARSET KOI8-R UID 2", false},
		{"ESEARCH CHARSET KOI8-R UID 2", false},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			rc := newSearchRev2Conn(t, multiSaveCaps())
			rc.appendOne("one")
			rc.appendOne("two")
			rc.do("SELECT INBOX")
			rc.do("UID SEARCH RETURN (SAVE) UID 1")

			resp, tagged := rc.try(tc.cmd)
			if !strings.Contains(tagged, " NO [BADCHARSET") || resp != tagged {
				t.Fatalf("want only tagged NO [BADCHARSET], got %q", resp)
			}

			check := rc.do("UID SEARCH RETURN (ALL) $")
			if tc.save && strings.Contains(check, " ALL ") {
				t.Errorf("$ kept the earlier result after a SAVE search failed with NO: %q", check)
			}
			if !tc.save && !strings.Contains(check, " ALL 1\r\n") {
				t.Errorf("a NO without SAVE changed $: %q", check)
			}
		})
	}
}

// A SAVE search that is BAD for another reason must stay BAD, and so keep $,
// whatever its CHARSET.
func TestSearchSaveBadCharsetBADKeepsSearchRes(t *testing.T) {
	for _, cmd := range []string{
		// RFC 7377 §2.2: SAVE with any source other than "selected" is BAD.
		"ESEARCH IN (inboxes) RETURN (SAVE) CHARSET KOI8-R UID 2",
		"ESEARCH IN (selected personal) RETURN (SAVE) CHARSET KOI8-R UID 2",
		// A syntax error after the CHARSET.
		"UID SEARCH RETURN (SAVE) CHARSET KOI8-R XYZZY",
	} {
		t.Run(cmd, func(t *testing.T) {
			rc := newSearchRev2Conn(t, multiSaveCaps())
			rc.appendOne("one")
			rc.appendOne("two")
			rc.do("SELECT INBOX")
			rc.do("UID SEARCH RETURN (SAVE) UID 1")

			resp, tagged := rc.try(cmd)
			if !strings.Contains(tagged, " BAD ") || resp != tagged {
				t.Fatalf("want only tagged BAD, got %q", resp)
			}
			if check := rc.do("UID SEARCH RETURN (ALL) $"); !strings.Contains(check, " ALL 1\r\n") {
				t.Errorf("BAD changed $: %q", check)
			}
		})
	}
}

// RFC 9051 §6.4.4.4 Example 5 sends the KOI8-R text as a literal. Sent
// non-synchronizing, its octets follow the command line without waiting for
// the server, so the server has to read them before it answers NO.
func TestSearchSaveBadCharsetLiteral(t *testing.T) {
	rc := newSearchRev2Conn(t, multiSaveCaps())
	rc.appendOne("one")
	rc.do("SELECT INBOX")
	rc.do("UID SEARCH RETURN (SAVE) UID 1")

	// "\xcd\xc1\xd4\xd8" is the KOI8-R encoding of "мать" (RFC 9051 §6.4.4.4
	// Example 4).
	resp, tagged := rc.try("UID SEARCH RETURN (SAVE) CHARSET KOI8-R (OR $ 1,3000:3021) TEXT {4+}\r\n\xcd\xc1\xd4\xd8")
	if !strings.Contains(tagged, " NO [BADCHARSET") || resp != tagged {
		t.Fatalf("want only tagged NO [BADCHARSET], got %q", resp)
	}
	if check := rc.do("UID SEARCH RETURN (ALL) $"); strings.Contains(check, " ALL ") {
		t.Errorf("$ kept the earlier result after a SAVE search failed with NO: %q", check)
	}
}

// searchResResetSession records the calls the server makes for an
// unsupported CHARSET. It does not implement SessionMultiSearch; see
// multiSearchResResetSession.
type searchResResetSession struct {
	imapserver.Session
	caps     imap.CapSet
	resets   atomic.Int32
	searches atomic.Int32
}

func (s *searchResResetSession) GetCapabilities() imap.CapSet { return s.caps }

func (s *searchResResetSession) ResetSearchRes() { s.resets.Add(1) }

func (s *searchResResetSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	s.searches.Add(1)
	return s.Session.Search(ctx, kind, criteria, options)
}

type multiSearchResResetSession struct {
	*searchResResetSession
}

func (s multiSearchResResetSession) MultiSearch(ctx context.Context, source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	s.searches.Add(1)
	return s.Session.(imapserver.SessionMultiSearch).MultiSearch(ctx, source, criteria, options)
}

// The server calls ResetSearchRes exactly when it fails a SAVE search with
// NO [BADCHARSET] itself, without calling the backend. A command that is BAD
// for another reason gets BAD and no call, whatever its CHARSET.
func TestSearchBadCharsetResetSearchRes(t *testing.T) {
	noSearchRes := multiSaveCaps()
	delete(noSearchRes, imap.CapSearchRes)
	noESearch := multiSaveCaps()
	delete(noESearch, imap.CapESearch)
	delete(noESearch, imap.CapSearchRes)

	for _, tc := range []struct {
		name       string
		cmd        string
		caps       imap.CapSet // session capabilities; multiSaveCaps if nil
		unselected bool
		noMulti    bool
		wantStatus string
		wantResets int32
	}{
		{name: "SEARCH SAVE", cmd: "SEARCH RETURN (SAVE) CHARSET KOI8-R ALL", wantStatus: "NO", wantResets: 1},
		{name: "UID SEARCH MIN SAVE", cmd: "UID SEARCH RETURN (MIN SAVE) CHARSET koi8-r ALL", wantStatus: "NO", wantResets: 1},
		{name: "ESEARCH SAVE", cmd: "ESEARCH RETURN (SAVE) CHARSET KOI8-R ALL", wantStatus: "NO", wantResets: 1},
		{name: "ESEARCH IN selected SAVE", cmd: "ESEARCH IN (SELECTED) RETURN (SAVE) CHARSET KOI8-R ALL", wantStatus: "NO", wantResets: 1},

		{name: "SEARCH without SAVE", cmd: "SEARCH CHARSET KOI8-R ALL", wantStatus: "NO"},
		{name: "SEARCH RETURN ALL", cmd: "SEARCH RETURN (ALL) CHARSET KOI8-R ALL", wantStatus: "NO"},
		{name: "ESEARCH personal without SAVE", cmd: "ESEARCH IN (personal) CHARSET KOI8-R ALL", wantStatus: "NO"},

		{name: "ESEARCH inboxes SAVE", cmd: "ESEARCH IN (INBOXES) RETURN (SAVE) CHARSET KOI8-R ALL", wantStatus: "BAD"},
		{name: "ESEARCH mixed sources SAVE", cmd: "ESEARCH IN (selected personal) RETURN (SAVE) CHARSET KOI8-R ALL", wantStatus: "BAD"},
		{name: "SEARCH syntax error", cmd: "SEARCH RETURN (SAVE) CHARSET KOI8-R XYZZY", wantStatus: "BAD"},
		{name: "SEARCH unselected", cmd: "SEARCH RETURN (SAVE) CHARSET KOI8-R ALL", unselected: true, wantStatus: "BAD"},
		{name: "SEARCH unselected without SAVE", cmd: "SEARCH CHARSET KOI8-R ALL", unselected: true, wantStatus: "BAD"},
		{name: "ESEARCH unselected", cmd: "ESEARCH RETURN (SAVE) CHARSET KOI8-R ALL", unselected: true, wantStatus: "BAD"},
		{name: "SEARCH SAVE without SEARCHRES", cmd: "SEARCH RETURN (SAVE) CHARSET KOI8-R ALL", caps: noSearchRes, wantStatus: "BAD"},
		{name: "ESEARCH SAVE without SEARCHRES", cmd: "ESEARCH RETURN (SAVE) CHARSET KOI8-R ALL", caps: noSearchRes, wantStatus: "BAD"},
		{name: "SEARCH RETURN without ESEARCH", cmd: "SEARCH RETURN (ALL) CHARSET KOI8-R ALL", caps: noESearch, wantStatus: "BAD"},
		{name: "ESEARCH without MULTISEARCH", cmd: "ESEARCH RETURN (SAVE) CHARSET KOI8-R ALL", noMulti: true, wantStatus: "BAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := tc.caps
			if caps == nil {
				caps = multiSaveCaps()
			}
			ready := make(chan *searchResResetSession, 1)
			rc := newSearchRev2ConnWrapped(t, multiSaveCaps(), func(s imapserver.Session) imapserver.Session {
				sess := &searchResResetSession{Session: s, caps: caps}
				ready <- sess
				if tc.noMulti {
					return sess
				}
				return multiSearchResResetSession{sess}
			})
			sess := <-ready
			rc.appendOne("one")
			if !tc.unselected {
				rc.do("SELECT INBOX")
			}

			resp, tagged := rc.try(tc.cmd)
			if !strings.Contains(tagged, " "+tc.wantStatus+" ") || resp != tagged {
				t.Errorf("response = %q, want only tagged %s", resp, tc.wantStatus)
			}
			if tc.wantStatus == "NO" && !strings.Contains(tagged, " NO [BADCHARSET") {
				t.Errorf("response = %q, want NO [BADCHARSET]", tagged)
			}
			if got := sess.resets.Load(); got != tc.wantResets {
				t.Errorf("ResetSearchRes calls = %d, want %d", got, tc.wantResets)
			}
			if got := sess.searches.Load(); got != 0 {
				t.Errorf("backend search calls = %d, want 0", got)
			}
			rc.do("NOOP")
		})
	}
}
