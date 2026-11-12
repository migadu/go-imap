package imapserver_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type idProbeSession struct {
	imapserver.Session
	ids chan *imap.IDData
}

func (s *idProbeSession) ID(data *imap.IDData) *imap.IDData {
	s.ids <- data
	return nil
}

// A failed NIL probe must not consume an arbitrary atom and then accept the
// following list as the complete ID argument.
func TestIDRejectsNonNILAtomBeforeList(t *testing.T) {
	for _, token := range []string{"NULL", "NILX", "nilx"} {
		for _, separator := range []string{"", " "} {
			name := token + "/adjacent"
			if separator != "" {
				name = token + "/spaced"
			}
			t.Run(name, func(t *testing.T) {
				ids := make(chan *imap.IDData, 1)
				w := dialParseServer(t, func(s imapserver.Session) imapserver.Session {
					return &idProbeSession{Session: s, ids: ids}
				})
				if _, err := fmt.Fprintf(w.conn, "id ID %s%s(\"name\" \"probe\")\r\nnext NOOP\r\n", token, separator); err != nil {
					t.Fatal(err)
				}
				if got := w.until("id"); !strings.HasPrefix(got, "id BAD ") {
					t.Errorf("invalid ID response = %q, want BAD", got)
				}
				if got := w.until("next"); !strings.HasPrefix(got, "next OK ") {
					t.Errorf("NOOP after invalid ID = %q, want OK", got)
				}
				select {
				case data := <-ids:
					t.Errorf("invalid ID reached the backend: %+v", data)
				default:
				}
			})
		}
	}
}

func TestIDArgumentProbePreservesValidArguments(t *testing.T) {
	for _, tc := range []struct {
		args     string
		wantNil  bool
		wantName string
	}{
		{args: "NIL", wantNil: true},
		{args: "nil", wantNil: true},
		{args: "nIl", wantNil: true},
		{args: "()"},
		{args: `("name" "probe")`, wantName: "probe"},
		{args: `("name" "NIL")`, wantName: "NIL"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			ids := make(chan *imap.IDData, 1)
			w := dialParseServer(t, func(s imapserver.Session) imapserver.Session {
				return &idProbeSession{Session: s, ids: ids}
			})
			if _, err := fmt.Fprintf(w.conn, "id ID %s\r\nnext NOOP\r\n", tc.args); err != nil {
				t.Fatal(err)
			}
			if got := w.until("id"); !strings.HasPrefix(got, "id OK ") {
				t.Fatalf("valid ID response = %q, want OK", got)
			}
			if got := w.until("next"); !strings.HasPrefix(got, "next OK ") {
				t.Errorf("NOOP after valid ID = %q, want OK", got)
			}
			select {
			case data := <-ids:
				if (data == nil) != tc.wantNil {
					t.Fatalf("backend ID data = %+v, want nil = %v", data, tc.wantNil)
				}
				if data != nil && data.Name != tc.wantName {
					t.Errorf("backend ID name = %q, want %q", data.Name, tc.wantName)
				}
			default:
				t.Error("valid ID did not reach the backend")
			}
		})
	}
}
