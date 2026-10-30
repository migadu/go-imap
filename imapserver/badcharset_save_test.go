package imapserver_test

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// RFC 5182 section 2.1: a SAVE search answered NO empties "$". The server layer
// answers an unsupported CHARSET with NO [BADCHARSET] before it calls the
// session, so it has to tell the session to empty "$" itself.
func TestBadCharsetSaveEmptiesSearchRes(t *testing.T) {
	for _, cmd := range []string{"UID SEARCH", "ESEARCH", "ESEARCH IN (selected)"} {
		t.Run(cmd, func(t *testing.T) {
			rc := newSearchRev2Conn(t, multiSaveCaps())
			rc.appendOne("one")
			rc.appendOne("two")
			rc.do("SELECT INBOX")

			rc.do("UID SEARCH RETURN (SAVE) UID 1")
			if _, tagged := rc.try(cmd + " RETURN (SAVE) CHARSET KOI8-R UID 2"); !strings.Contains(tagged, " NO [BADCHARSET]") {
				t.Fatalf("SAVE with an unsupported charset = %q, want NO [BADCHARSET]", tagged)
			}
			if check := rc.do("UID SEARCH RETURN (ALL) $"); strings.Contains(check, " ALL ") {
				t.Errorf("$ after NO [BADCHARSET] = %q, want it empty", check)
			}

			// A search without SAVE leaves "$" as it was, whatever its outcome.
			rc.do("UID SEARCH RETURN (SAVE) UID 1")
			if _, tagged := rc.try(cmd + " CHARSET KOI8-R UID 2"); !strings.Contains(tagged, " NO [BADCHARSET]") {
				t.Fatalf("unsupported charset = %q, want NO [BADCHARSET]", tagged)
			}
			if check := rc.do("UID SEARCH RETURN (ALL) $"); !strings.Contains(check, " ALL 1\r\n") {
				t.Errorf("$ after a failed search without SAVE = %q, want 1", check)
			}
		})
	}
}

// A search that is BAD for a reason other than its charset stays BAD with an
// unsupported charset too: a BAD keeps "$" where NO would empty it (RFC 5182
// section 2.1), and RFC 4466 section 2.6.1 makes unsupported options BAD.
func TestBadCharsetAfterBadChecks(t *testing.T) {
	noSearchRes := multiSaveCaps()
	delete(noSearchRes, imap.CapSearchRes)

	for _, tc := range []struct {
		name, cmd string
		caps      imap.CapSet
		selected  bool
	}{
		{"SEARCH SAVE without SEARCHRES", "UID SEARCH RETURN (SAVE) CHARSET KOI8-R ALL", noSearchRes, true},
		{"ESEARCH SAVE without SEARCHRES", "ESEARCH RETURN (SAVE) CHARSET KOI8-R ALL", noSearchRes, true},
		{"ESEARCH SAVE from another source", "ESEARCH IN (personal) RETURN (SAVE) CHARSET KOI8-R ALL", multiSaveCaps(), true},
		{"SEARCH not selected", "UID SEARCH RETURN (SAVE) CHARSET KOI8-R ALL", multiSaveCaps(), false},
		{"ESEARCH SAVE not selected", "ESEARCH RETURN (SAVE) CHARSET KOI8-R ALL", multiSaveCaps(), false},
		{"SEARCH result option without ESEARCH", "UID SEARCH RETURN (COUNT) CHARSET KOI8-R ALL", imap.CapSet{imap.CapIMAP4rev1: {}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, tc.caps)
			rc.appendOne("one")
			saves := tc.selected && tc.caps.Has(imap.CapSearchRes)
			if tc.selected {
				rc.do("SELECT INBOX")
			}
			if saves {
				rc.do("UID SEARCH RETURN (SAVE) UID 1")
			}

			if _, tagged := rc.try(tc.cmd); !strings.Contains(tagged, " BAD ") {
				t.Errorf("%s = %q, want BAD", tc.cmd, tagged)
			}
			if saves {
				if check := rc.do("UID SEARCH RETURN (ALL) $"); !strings.Contains(check, " ALL 1\r\n") {
					t.Errorf("$ after a BAD = %q, want 1", check)
				}
			}
		})
	}
}
