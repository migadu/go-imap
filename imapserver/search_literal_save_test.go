package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// A server-side literal refusal is still a SAVE search answered NO, even
// before the backend sees it (RFC 5182 section 2.1). Test the actual saved set
// and the hook count, including decoder errors wrapped by the search parser.
func TestSearchLiteralFailureSearchRes(t *testing.T) {
	for _, command := range []string{"SEARCH", "UID SEARCH", "ESEARCH", "ESEARCH IN (selected)"} {
		for _, tc := range []struct {
			name, prefix string
			save         bool
		}{
			{"unsupported charset", "RETURN (SAVE) CHARSET KOI8-R TEXT ", true},
			{"supported charset", "RETURN (COUNT SAVE) CHARSET UTF-8 TEXT ", true},
			{"default charset", "RETURN (SAVE) TEXT ", true},
			{"charset literal", "RETURN (SAVE) CHARSET ", true},
			{"date literal", "RETURN (SAVE) CHARSET KOI8-R SINCE ", true},
			{"no SAVE", "CHARSET KOI8-R TEXT ", false},
		} {
			for _, nonSync := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/nonSync=%t", command, tc.name, nonSync), func(t *testing.T) {
					caps := multiSaveCaps()
					caps[imap.CapLiteralPlus] = struct{}{}
					ready := make(chan *searchLiteralSession, 1)
					rc := newSearchRev2ConnWrapped(t, caps, func(s imapserver.Session) imapserver.Session {
						sess := &searchLiteralSession{Session: s}
						ready <- sess
						return sess
					})
					sess := <-ready
					rc.appendOne("one")
					rc.do("SELECT INBOX")
					rc.do("UID SEARCH RETURN (SAVE) UID 1")
					sess.searches.Store(0)

					literal := "{4097}"
					if nonSync {
						// Command-looking bytes must be drained as literal data.
						payload := "\r\ninjected LOGOUT\r\n"
						payload += strings.Repeat("x", 4097-len(payload))
						literal = "{4097+}\r\n" + payload
						if tc.name == "charset literal" {
							literal += " ALL"
						}
					}
					resp, tagged := rc.try(command + " " + tc.prefix + literal)
					if !strings.Contains(tagged, " NO [TOOBIG]") || resp != tagged {
						t.Fatalf("want only tagged NO [TOOBIG], got %q", resp)
					}
					wantResets := int32(0)
					if tc.save {
						wantResets = 1
					}
					if got := sess.resets.Load(); got != wantResets {
						t.Errorf("ResetSearchRes calls = %d, want %d", got, wantResets)
					}
					if got := sess.searches.Load(); got != 0 {
						t.Errorf("backend searches = %d, want 0", got)
					}
					check := rc.do("UID SEARCH RETURN (ALL) $")
					if tc.save && strings.Contains(check, " ALL ") {
						t.Errorf("SAVE answered NO retained old $: %q", check)
					}
					if !tc.save && !strings.Contains(check, " ALL 1\r\n") {
						t.Errorf("NO without SAVE changed $: %q", check)
					}
					rc.do("NOOP")
				})
			}
		}
	}
}

type searchLiteralSession struct {
	imapserver.Session
	resets, searches atomic.Int32
	backendError     error
}

func (s *searchLiteralSession) ResetSearchRes() {
	s.resets.Add(1)
	s.Session.(imapserver.SessionSearchRes).ResetSearchRes()
}

func (s *searchLiteralSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	s.searches.Add(1)
	if s.backendError != nil {
		return nil, s.backendError
	}
	return s.Session.Search(ctx, kind, criteria, options)
}

func (s *searchLiteralSession) MultiSearch(ctx context.Context, source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	s.searches.Add(1)
	if s.backendError != nil {
		return nil, s.backendError
	}
	return s.Session.(imapserver.SessionMultiSearch).MultiSearch(ctx, source, criteria, options)
}

// A backend that executes the search owns its SAVE failure handling. The
// frontend hook must not run again; nor may it dereference a nil selected view.
func TestSearchLiteralResetOwnership(t *testing.T) {
	for _, command := range []string{"SEARCH", "ESEARCH"} {
		for _, backend := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/backend=%t", command, backend), func(t *testing.T) {
				ready := make(chan *searchLiteralSession, 1)
				rc := newSearchRev2ConnWrapped(t, multiSaveCaps(), func(s imapserver.Session) imapserver.Session {
					sess := &searchLiteralSession{Session: s}
					if backend {
						sess.backendError = &imap.Error{Type: imap.StatusResponseTypeNo, Code: "NOTSAVED", Text: "Search refused by backend"}
					}
					ready <- sess
					return sess
				})
				sess := <-ready
				criteria := "TEXT {4097}"
				if backend {
					rc.do("SELECT INBOX")
					criteria = "ALL"
				}
				resp, tagged := rc.try(command + " RETURN (SAVE) " + criteria)
				if !strings.Contains(tagged, " NO ") || resp != tagged {
					t.Fatalf("want tagged NO, got %q", resp)
				}
				if got := sess.resets.Load(); got != 0 {
					t.Errorf("frontend reset calls = %d, want 0", got)
				}
				wantSearches := int32(0)
				if backend {
					wantSearches = 1
				}
				if got := sess.searches.Load(); got != wantSearches {
					t.Errorf("backend searches = %d, want %d", got, wantSearches)
				}
				rc.do("NOOP")
			})
		}
	}
}
