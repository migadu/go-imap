package imapserver_test

import (
	"fmt"
	"strings"
	"testing"
)

// A client's flag is an atom, which excludes "]". imapclient tolerates "]" in
// the flag lists servers send, but the server must not take such a keyword
// from a client: it would then list it in FLAGS and PERMANENTFLAGS, which a
// strict client cannot parse.
func TestStoreKeywordWithBracketIsBad(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{"bare", `a3 STORE 1 +FLAGS foo]`},
		{"list", `a3 STORE 1 +FLAGS (foo])`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := dialParseServer(t, nil)
			fmt.Fprint(w.conn, "a2 SELECT INBOX\r\n")
			w.until("a2")

			fmt.Fprint(w.conn, tc.cmd+"\r\n")
			if got := w.until("a3"); !strings.HasPrefix(got, "a3 BAD") {
				t.Errorf("answer was %q; want BAD", got)
			}
		})
	}
}
