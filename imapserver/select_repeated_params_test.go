package imapserver_test

import (
	"strings"
	"testing"
)

func TestSelectRepeatedParametersAreBAD(t *testing.T) {
	for _, tc := range []struct {
		command string
		enable  bool
	}{
		{command: "SELECT INBOX (CONDSTORE CONDSTORE)"},
		{command: "EXAMINE INBOX (CONDSTORE CONDSTORE)"},
		{command: "SELECT INBOX (QRESYNC (1 1) QRESYNC (2 2))", enable: true},
		{command: "EXAMINE INBOX (QRESYNC (1 1 1:3) QRESYNC (999 1))", enable: true},
		{command: "SELECT INBOX (CONDSTORE QRESYNC (1 1) CONDSTORE)", enable: true},
		{command: "SELECT INBOX (QRESYNC (1 1) CONDSTORE QRESYNC (2 2))", enable: true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			rc := newQResyncTestConn(t)

			if tc.enable {
				rc.send("a1 ENABLE QRESYNC")
				rc.readUntilTag("a1")
			}

			rc.send("a2 " + tc.command)
			resp := rc.readUntilTag("a2")
			if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a2 BAD [CLIENTBUG] ") || len(resp) != 1 {
				t.Errorf("%v = %q, want BAD [CLIENTBUG] and no mailbox data:\n\t%s", tc.command, tagged, strings.Join(resp, "\n\t"))
			}

			rc.send("a3 CLOSE")
			resp = rc.readUntilTag("a3")
			if tagged := resp[len(resp)-1]; strings.HasPrefix(tagged, "a3 OK") {
				t.Errorf("CLOSE after refused %v = %q, want no mailbox selected", tc.command, tagged)
			}
			rc.send("a4 NOOP")
			resp = rc.readUntilTag("a4")
			if tagged := resp[len(resp)-1]; !strings.HasPrefix(tagged, "a4 OK") {
				t.Errorf("NOOP after refused %v = %q, want OK", tc.command, tagged)
			}
		})
	}
}
