package imapclient_test

import (
	"testing"

	"github.com/migadu/go-imap"
)

func TestBodyStructureNILCase(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"upper case control", `("TEXT" "PLAIN" NIL NIL NIL "7BIT" 7 1)`},
		{"lower params", `("TEXT" "PLAIN" nil NIL NIL "7BIT" 7 1)`},
		{"mixed ID", `("TEXT" "PLAIN" NIL nIl NIL "7BIT" 7 1)`},
		{"lower description", `("TEXT" "PLAIN" NIL NIL nil "7BIT" 7 1)`},
		{"QQ lower type", `(nil NIL ("CHARSET" "GB2312") NIL NIL NIL 7 NIL NIL NIL)`},
		{"QQ mixed subtype", `(NIL NiL ("CHARSET" "GB2312") NIL NIL NIL 7 NIL NIL NIL)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newSelectedCodeServer(t, "* 1 FETCH (BODYSTRUCTURE "+tc.body+")\r\n")
			messages, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}).Collect()
			if err != nil || len(messages) != 1 {
				t.Fatalf("Fetch()=(%v,%v), want one message", messages, err)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop() after FETCH: %v", err)
			}
		})
	}
}

func TestMetadataNILCaseAndStringData(t *testing.T) {
	client := newCodeServer(t, "* METADATA INBOX (/private/absent nil /private/mixed nIl /private/quoted \"nil\" /private/literal {3}\r\nNiL)\r\n")
	client.SetStateForTest(imap.ConnStateAuthenticated)
	data, err := client.GetMetadata("INBOX", []string{"/private/absent", "/private/mixed", "/private/quoted", "/private/literal"}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/private/absent", "/private/mixed"} {
		if value, ok := data.Entries[name]; !ok || value != nil {
			t.Errorf("%s=%v, present=%v; want nil", name, value, ok)
		}
	}
	for name, want := range map[string]string{"/private/quoted": "nil", "/private/literal": "NiL"} {
		if value := data.Entries[name]; value == nil || string(*value) != want {
			t.Errorf("%s=%v, want %q", name, value, want)
		}
	}
	if err := client.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
}
