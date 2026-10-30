package imapserver_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// A refused literal stops the parse before the checks that answer BAD, but IN
// and RETURN come before the search program, so they can already make a SAVE
// search BAD. RFC 7377 §2.2 requires BAD for SAVE with any source other than
// "selected", and a BAD must leave "$" unchanged (RFC 5182 §2.1); NO [TOOBIG]
// would empty it.
func TestSearchLiteralRefusalInvalidSaveKeepsSearchRes(t *testing.T) {
	for _, prefix := range []string{
		"ESEARCH IN (personal) RETURN (SAVE) TEXT ",
		"ESEARCH IN (inboxes) RETURN (SAVE) TEXT ",
		"ESEARCH IN (selected personal) RETURN (SAVE) TEXT ",
		"ESEARCH IN (mailboxes INBOX) RETURN (SAVE) CHARSET UTF-8 TEXT ",
		"ESEARCH IN (subtree INBOX) RETURN (COUNT SAVE) CHARSET ",
	} {
		for _, nonSync := range []bool{false, true} {
			literal := "{4097}"
			if nonSync {
				// Command-looking bytes must be drained as literal data.
				payload := "\r\ninjected LOGOUT\r\n"
				payload += strings.Repeat("x", 4097-len(payload))
				literal = "{4097+}\r\n" + payload + " ALL"
			}
			t.Run(fmt.Sprintf("%s/nonSync=%t", prefix, nonSync), func(t *testing.T) {
				caps := multiSaveCaps()
				caps[imap.CapLiteralPlus] = struct{}{}
				rc := newSearchRev2Conn(t, caps)
				rc.appendOne("one")
				rc.do("SELECT INBOX")
				rc.do("UID SEARCH RETURN (SAVE) UID 1")

				resp, tagged := rc.try(prefix + literal)
				if !strings.Contains(tagged, " BAD [CLIENTBUG] ESEARCH SAVE requires the selected source") || resp != tagged {
					t.Errorf("want only tagged BAD [CLIENTBUG], got %q", resp)
				}
				if check := rc.do("UID SEARCH RETURN (ALL) $"); !strings.Contains(check, " ALL 1\r\n") {
					t.Errorf("BAD changed $: %q", check)
				}
				rc.do("NOOP")
			})
		}
	}
}

// The same holds for the other reasons a SAVE search is BAD: options the
// server does not support (RFC 4466 §2.6.1) and ESEARCH without
// SessionMultiSearch. Valid SAVE searches still get NO [TOOBIG] and empty "$".
func TestSearchLiteralRefusalResetSearchRes(t *testing.T) {
	noSearchRes := multiSaveCaps()
	delete(noSearchRes, imap.CapSearchRes)
	noESearch := multiSaveCaps()
	delete(noESearch, imap.CapESearch)

	for _, tc := range []struct {
		name       string
		cmd        string
		caps       imap.CapSet // session capabilities; multiSaveCaps if nil
		noMulti    bool
		want       string
		wantResets int32
	}{
		{name: "SEARCH SAVE without SEARCHRES", cmd: "SEARCH RETURN (SAVE) TEXT {4097}", caps: noSearchRes, want: " BAD [CLIENTBUG] SEARCH SAVE requires SEARCHRES"},
		{name: "ESEARCH SAVE without SEARCHRES", cmd: "ESEARCH RETURN (SAVE) TEXT {4097}", caps: noSearchRes, want: " BAD [CLIENTBUG] SEARCH SAVE requires SEARCHRES"},
		{name: "SEARCH SAVE without ESEARCH", cmd: "UID SEARCH RETURN (SAVE) TEXT {4097}", caps: noESearch, want: " BAD [CLIENTBUG] SEARCH result options require ESEARCH"},
		{name: "ESEARCH without MULTISEARCH", cmd: "ESEARCH IN (selected) RETURN (SAVE) TEXT {4097}", noMulti: true, want: " BAD MULTISEARCH not supported"},

		{name: "SEARCH SAVE", cmd: "SEARCH RETURN (SAVE) TEXT {4097}", want: " NO [TOOBIG] ", wantResets: 1},
		{name: "ESEARCH SAVE", cmd: "ESEARCH RETURN (SAVE) TEXT {4097}", want: " NO [TOOBIG] ", wantResets: 1},
		{name: "ESEARCH IN selected SAVE", cmd: "ESEARCH IN (selected) RETURN (SAVE) TEXT {4097}", want: " NO [TOOBIG] ", wantResets: 1},
		// Without SAVE, "$" is not at stake and the refusal stays NO.
		{name: "SEARCH RETURN without ESEARCH", cmd: "SEARCH RETURN (ALL) TEXT {4097}", caps: noESearch, want: " NO [TOOBIG] "},
		{name: "ESEARCH personal without SAVE", cmd: "ESEARCH IN (personal) TEXT {4097}", want: " NO [TOOBIG] "},
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
			rc.do("SELECT INBOX")

			resp, tagged := rc.try(tc.cmd)
			if !strings.Contains(tagged, tc.want) || resp != tagged {
				t.Errorf("response = %q, want only tagged %q", resp, tc.want)
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
