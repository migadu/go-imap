package imapserver_test

import (
	"strings"
	"testing"
)

// A malformed date is a command syntax error, including when CHARSET is
// unsupported. BAD must preserve the saved result (RFC 9051 sections 7.1.3 and
// 6.4.4.1), whereas a valid command rejected with NO [BADCHARSET] clears it.
func TestSearchInvalidDateKeepsSearchRes(t *testing.T) {
	for _, command := range []string{"SEARCH", "UID SEARCH", "ESEARCH"} {
		for _, charset := range []string{"", "CHARSET UTF-8 ", "CHARSET KOI8-R "} {
			t.Run(command+"/"+charset, func(t *testing.T) {
				rc := newSearchRev2Conn(t, multiSaveCaps())
				rc.appendOne("one")
				rc.appendOne("two")
				rc.do("SELECT INBOX")
				rc.do("UID SEARCH RETURN (SAVE) UID 1")

				for _, date := range []string{"nonsense", `"31-Feb-2024"`} {
					cmd := command + " RETURN (SAVE) " + charset + "SINCE " + date
					resp, tagged := rc.try(cmd)
					if !strings.Contains(tagged, " BAD [CLIENTBUG] ") || resp != tagged {
						t.Errorf("%s: want only tagged BAD [CLIENTBUG], got %q", cmd, resp)
					}
					if check := rc.do("UID SEARCH RETURN (ALL) $"); !strings.Contains(check, " ALL 1\r\n") {
						t.Errorf("%s: malformed date changed $: %q", cmd, check)
					}
				}

				// A valid date still reaches ordinary SEARCH or BADCHARSET
				// handling. On success the different result proves the backend ran.
				cmd := command + " RETURN (SAVE) " + charset + "SINCE 1-Jan-2000 UID 2"
				resp, tagged := rc.try(cmd)
				if strings.Contains(charset, "KOI8-R") {
					if !strings.Contains(tagged, " NO [BADCHARSET] ") || resp != tagged {
						t.Errorf("%s: want only tagged NO [BADCHARSET], got %q", cmd, resp)
					}
					if check := rc.do("UID SEARCH RETURN (ALL) $"); strings.Contains(check, " ALL ") {
						t.Errorf("%s: NO [BADCHARSET] retained $: %q", cmd, check)
					}
				} else {
					if !strings.Contains(tagged, " OK ") || resp != tagged {
						t.Errorf("%s: want only tagged OK, got %q", cmd, resp)
					}
					if check := rc.do("UID SEARCH RETURN (ALL) $"); !strings.Contains(check, " ALL 2\r\n") {
						t.Errorf("%s: valid date failed to update $: %q", cmd, check)
					}
				}
				rc.do("NOOP")
			})
		}
	}
}
