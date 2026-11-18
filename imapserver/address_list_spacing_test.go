package imapserver

import (
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// TestWriteEnvelopeAddressListNoSP: RFC 3501 §9 and RFC 9051 §9 define every envelope
// address list as "(" 1*address ")" with no SP between the addresses (#377), unlike
// "(" x *(SP x) ")" lists such as body-fld-param.
func TestWriteEnvelopeAddressListNoSP(t *testing.T) {
	got := encodeForTest(func(enc *imapwire.Encoder) {
		writeEnvelope(enc, &imap.Envelope{
			Subject: "s",
			From:    []imap.Address{{Name: "Alice", Mailbox: "alice", Host: "example.com"}},
			To: []imap.Address{
				{Mailbox: "g1", Host: "d1.org"},
				{Mailbox: "g2", Host: "d2.org"},
			},
			Cc: []imap.Address{{Mailbox: "team"}, {Mailbox: "c", Host: "example.com"}, {}},
		})
	})
	for _, want := range []string{
		` ((NIL NIL "g1" "d1.org")(NIL NIL "g2" "d2.org")) `,
		` ((NIL NIL "team" NIL)(NIL NIL "c" "example.com")(NIL NIL NIL NIL)) `,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("writeEnvelope() = %s, want it to contain %s", got, want)
		}
	}
}

// TestWriteNamespaceNoSP: a NAMESPACE list is "(" 1*namespace-descr ")" (RFC 9051 §9,
// RFC 2342 §6), with no SP between the descriptors.
func TestWriteNamespaceNoSP(t *testing.T) {
	got := encodeForTest(func(enc *imapwire.Encoder) {
		writeNamespace(enc, []imap.NamespaceDescriptor{
			{Prefix: "", Delim: '/'},
			{Prefix: "Shared/", Delim: '/'},
		})
	})
	if want := `(("" "/")("Shared/" "/"))`; got != want {
		t.Errorf("writeNamespace() = %s, want %s", got, want)
	}
}
