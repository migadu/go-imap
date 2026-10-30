package imapserver_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// capsChangedBySearchSession stops reporting ESEARCH once Search has run, as
// a session that filters its capabilities by client behaviour may.
type capsChangedBySearchSession struct {
	imapserver.Session
	searched atomic.Bool
}

func (s *capsChangedBySearchSession) GetCapabilities() imap.CapSet {
	if s.searched.Load() {
		return imap.CapSet{imap.CapIMAP4rev1: {}}
	}
	return imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}}
}

func (s *capsChangedBySearchSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	s.searched.Store(true)
	return s.Session.Search(ctx, kind, criteria, options)
}

// The ESEARCH support that accepted the result options must also choose the
// response: RFC 4731 §3.1 answers them with "a single ESEARCH response ...
// instead of the SEARCH response", which a plain SEARCH cannot carry.
func TestSearchResponseFormUsesCapsBeforeBackend(t *testing.T) {
	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}}
	for _, cmd := range []string{
		"SEARCH RETURN (COUNT) ALL",
		"UID SEARCH RETURN (MIN MAX) ALL",
		"SEARCH RETURN () ALL",
	} {
		t.Run(cmd, func(t *testing.T) {
			rc := newSearchRev2ConnWrapped(t, caps, func(s imapserver.Session) imapserver.Session {
				return &capsChangedBySearchSession{Session: s}
			})
			rc.appendOne("one")
			rc.do("SELECT INBOX")
			if resp := rc.do(cmd); !strings.Contains(resp, "* ESEARCH ") || strings.Contains(resp, "* SEARCH") {
				t.Errorf("want an ESEARCH response, got %q", resp)
			}
		})
	}
}
