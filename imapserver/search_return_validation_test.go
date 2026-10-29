package imapserver_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

type searchReturnSession struct {
	imapserver.SessionIMAP4rev2
	caps      imap.CapSet
	searchErr error
	searches  atomic.Int32
	sorts     atomic.Int32
}

func (s *searchReturnSession) GetCapabilities() imap.CapSet { return s.caps }

func (s *searchReturnSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	s.searches.Add(1)
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return s.SessionIMAP4rev2.Search(ctx, kind, criteria, options)
}

func (s *searchReturnSession) Sort(ctx context.Context, kind imapserver.NumKind, criteria []imap.SortCriterion, charset string, search *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	s.sorts.Add(1)
	return s.SessionIMAP4rev2.Sort(ctx, kind, criteria, charset, search, options)
}

func newSearchReturnConn(t *testing.T, serverCaps, sessionCaps imap.CapSet, searchErr error) (*searchRev2Conn, *searchReturnSession) {
	t.Helper()
	ready := make(chan *searchReturnSession, 1)
	rc := newSearchRev2ConnWrapped(t, serverCaps, func(s imapserver.Session) imapserver.Session {
		sess := &searchReturnSession{SessionIMAP4rev2: s.(imapserver.SessionIMAP4rev2), caps: sessionCaps, searchErr: searchErr}
		ready <- sess
		return sess
	})
	return rc, <-ready
}

func TestUnknownSearchAndSortReturnOptions(t *testing.T) {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {}, imap.CapSort: {}, imap.CapESort: {}}
	for _, cmd := range []string{
		"SEARCH RETURN (XYZZY) ALL",
		"UID SEARCH RETURN (ALL XYZZY) ALL",
		"SEARCH RETURN (xyzzy COUNT) ALL",
		"SORT RETURN (XYZZY) (ARRIVAL) UTF-8 ALL",
		"UID SORT RETURN (COUNT XYZZY) (ARRIVAL) UTF-8 ALL",
		// SORT has no SAVE implementation; accepting it must not silently
		// turn it into RETURN (ALL).
		"SORT RETURN (SAVE) (ARRIVAL) UTF-8 ALL",
		"SEARCH RETURN (XYZZY (\"parameter\" 1)) ALL",
		"SORT RETURN (XYZZY (\"parameter\" 1)) (ARRIVAL) UTF-8 ALL",
	} {
		t.Run(cmd, func(t *testing.T) {
			rc, sess := newSearchReturnConn(t, caps, caps, nil)
			rc.appendOne("one")
			rc.do("SELECT INBOX")
			resp, tagged := rc.try(cmd)
			if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
				t.Errorf("want BAD [CLIENTBUG], got %q", resp)
			}
			if sess.searches.Load() != 0 || sess.sorts.Load() != 0 {
				t.Errorf("rejected option reached backend: search=%d sort=%d", sess.searches.Load(), sess.sorts.Load())
			}
			rc.do("NOOP")
		})
	}
}

func TestUnknownReturnLiteralRecovery(t *testing.T) {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSort: {}, imap.CapESort: {}}
	for _, command := range []string{"SEARCH", "SORT"} {
		t.Run(command+" synchronizing literal", func(t *testing.T) {
			rc, sess := newSearchReturnConn(t, caps, caps, nil)
			rc.do("SELECT INBOX")
			// A rejected synchronizing literal gets no continuation: the
			// client sends no payload and may send its next command instead.
			_, tagged := rc.try(command + " RETURN (XYZZY {3}")
			if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
				t.Fatalf("want BAD, got %q", tagged)
			}
			rc.do("NOOP")
			if sess.searches.Load()+sess.sorts.Load() != 0 {
				t.Fatal("rejected command reached backend")
			}
		})
		t.Run(command+" non-synchronizing literal", func(t *testing.T) {
			rc, sess := newSearchReturnConn(t, caps, caps, nil)
			rc.do("SELECT INBOX")
			// DiscardLine cannot recover an unparsed non-sync literal. The
			// safe existing policy is BYE, never executing payload as commands.
			payload := "injected NOOP\r\n"
			wire := fmt.Sprintf("bad %s RETURN (XYZZY {%d+}\r\n%s) ALL\r\n", command, len(payload), payload)
			if _, err := io.WriteString(rc.c, wire); err != nil {
				t.Fatal(err)
			}
			response, err := io.ReadAll(io.LimitReader(rc.br, 4096))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(response), "* BYE ") || strings.Contains(string(response), "injected OK") {
				t.Fatalf("want safe close without executing payload, got %q", response)
			}
			if sess.searches.Load()+sess.sorts.Load() != 0 {
				t.Fatal("rejected command reached backend")
			}
		})
	}
}

func TestSearchSaveRequiresSearchRes(t *testing.T) {
	withSave := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {}}
	withoutSave := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}}
	for _, tc := range []struct {
		name string
		caps imap.CapSet
	}{
		{"server capability", withoutSave},
		{"session capability", withSave},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rc *searchRev2Conn
			var sess *searchReturnSession
			if tc.name == "server capability" {
				rc = newSearchRev2Conn(t, tc.caps)
			} else {
				rc, sess = newSearchReturnConn(t, tc.caps, withoutSave, nil)
			}
			rc.appendOne("one")
			rc.do("SELECT INBOX")
			for _, cmd := range []string{"SEARCH RETURN (SAVE) ALL", "UID SEARCH RETURN (ALL SAVE) ALL"} {
				resp, tagged := rc.try(cmd)
				if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
					t.Errorf("%s: want BAD, got %q", cmd, resp)
				}
			}
			if sess != nil && sess.searches.Load() != 0 {
				t.Error("unsupported SAVE reached backend")
			}
			rc.do("NOOP")
		})
	}
}

func TestSearchSaveOnlySuppressesResults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session bool
	}{
		{"rev1", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {}, imap.CapCondStore: {}}, false},
		{"rev2 implied SEARCHRES", imap.CapSet{imap.CapIMAP4rev2: {}, imap.CapCondStore: {}}, false},
		{"session adds SEARCHRES", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {}, imap.CapCondStore: {}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rc *searchRev2Conn
			if tc.session {
				rc, _ = newSearchReturnConn(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}}, tc.caps, nil)
			} else {
				rc = newSearchRev2Conn(t, tc.caps)
			}
			rc.appendOne("one")
			rc.appendOne("two")
			rc.do("SELECT INBOX")
			for _, prefix := range []string{"SEARCH", "UID SEARCH"} {
				for _, search := range []struct{ key, want string }{
					{"SUBJECT one", " ALL 1\r\n"},
					{"MODSEQ 1", " ALL 1:2\r\n"},
					{"SUBJECT missing", ""},
				} {
					cmd := prefix + " RETURN (SAVE) " + search.key
					resp, tagged := rc.try(cmd)
					if !strings.Contains(tagged, " OK ") || resp != tagged {
						t.Errorf("%s: want only tagged OK, got %q", cmd, resp)
					}
					resp = rc.do("UID SEARCH RETURN (ALL) $")
					if search.want == "" {
						if strings.Contains(resp, " ALL ") {
							t.Errorf("empty SAVE did not clear result: %q", resp)
						}
					} else if !strings.Contains(resp, search.want) {
						t.Errorf("SAVE did not persist result: want %q, got %q", search.want, resp)
					}
				}
			}
			for _, ret := range []string{"COUNT SAVE", "SAVE ALL"} {
				resp := rc.do("SEARCH RETURN (" + ret + ") ALL")
				if !strings.Contains(resp, "* ESEARCH ") {
					t.Errorf("mixed SAVE suppressed requested results: %q", resp)
				}
			}
		})
	}
}

func TestSearchSaveOnlyPreservesBackendError(t *testing.T) {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.CapSearchRes: {}}
	backendErr := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCode("NOTSAVED"), Text: "cannot save result"}
	rc, sess := newSearchReturnConn(t, caps, caps, backendErr)
	rc.do("SELECT INBOX")
	resp, tagged := rc.try("SEARCH RETURN (SAVE) ALL")
	if !strings.Contains(tagged, " NO [NOTSAVED] ") || resp != tagged {
		t.Fatalf("want backend NO without results, got %q", resp)
	}
	if sess.searches.Load() != 1 {
		t.Fatalf("SAVE must call backend, got %d calls", sess.searches.Load())
	}
}
