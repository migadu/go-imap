package imapserver_test

import (
	"strings"
	"testing"
)

// RFC 5182 §2.4 and RFC 9051 §6.4.4.1: SAVE with MIN/MAX alone saves only
// those endpoint messages. ALL or COUNT instead makes SAVE retain every match.
// The saved variable contains messages, so it must work in both UID and
// sequence-number commands even when the two number spaces differ.
func TestSearchSaveMinMax(t *testing.T) {
	for _, command := range []string{"SEARCH", "UID SEARCH", "ESEARCH", "ESEARCH IN (selected)"} {
		t.Run(command, func(t *testing.T) {
			rc := newSearchRev2Conn(t, multiSaveCaps())
			for _, subject := range []string{"discard", "outside lower", "match first", "match middle", "match last", "outside upper"} {
				rc.appendOne(subject)
			}
			rc.do("SELECT INBOX")
			rc.do("STORE 1 +FLAGS.SILENT (\\Deleted)")
			if resp := rc.do("EXPUNGE"); !strings.Contains(resp, "* 1 EXPUNGE\r\n") {
				t.Fatalf("failed to separate sequence numbers from UIDs: %q", resp)
			}
			// Matches are now sequence numbers 2:4, UIDs 3:5. The mailbox
			// also has nonmatching messages before and after those matches.
			for _, tc := range []struct {
				options, criteria, savedUIDs, savedSeqs string
			}{
				{"SAVE MIN", "SUBJECT match", "3", "2"},
				{"SAVE MAX", "SUBJECT match", "5", "4"},
				{"SAVE MIN MAX", "SUBJECT match", "3,5", "2,4"},
				{"SAVE MIN ALL", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MAX ALL", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MIN MAX ALL", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MIN COUNT", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MAX COUNT", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MIN MAX COUNT", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MIN MAX ALL COUNT", "SUBJECT match", "3:5", "2:4"},
				{"SAVE", "SUBJECT match", "3:5", "2:4"},
				{"SAVE MIN", "UID 4", "4", "3"},
				{"SAVE MAX", "UID 4", "4", "3"},
				{"SAVE MIN MAX", "UID 4", "4", "3"},
				{"SAVE MIN", "UID 99", "", ""},
				{"SAVE MAX", "UID 99", "", ""},
				{"SAVE MIN MAX", "UID 99", "", ""},
				{"SAVE MIN MAX ALL", "UID 99", "", ""},
				{"SAVE MIN MAX COUNT", "UID 99", "", ""},
			} {
				t.Run(tc.options+"/"+tc.criteria, func(t *testing.T) {
					// An empty result must replace an existing saved result,
					// not just leave a variable that was already empty alone.
					rc.do("UID SEARCH RETURN (SAVE) UID 2")
					rc.do(command + " RETURN (" + tc.options + ") " + tc.criteria)
					assertSavedSearchSet(t, rc.do("UID SEARCH RETURN (ALL) $"), tc.savedUIDs)
					assertSavedSearchSet(t, rc.do("SEARCH RETURN (ALL) $"), tc.savedSeqs)
					// Selected ESEARCH must read the same saved variable.
					assertSavedSearchSet(t, rc.do("ESEARCH IN (selected) RETURN (ALL) $"), tc.savedUIDs)
				})
			}

			// Computing endpoints without SAVE must preserve the saved set.
			rc.do("UID SEARCH RETURN (SAVE) UID 2")
			rc.do(command + " RETURN (MIN MAX) SUBJECT match")
			assertSavedSearchSet(t, rc.do("UID SEARCH RETURN (ALL) $"), "2")
		})
	}
}

func assertSavedSearchSet(t *testing.T, response, want string) {
	t.Helper()
	if want == "" {
		if strings.Contains(response, " ALL ") {
			t.Errorf("saved result = %q, want empty", response)
		}
	} else if !strings.Contains(response, " ALL "+want+"\r\n") {
		t.Errorf("saved result = %q, want %s", response, want)
	}
}
