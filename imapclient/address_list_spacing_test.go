package imapclient

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// TestReadAddressListAndNamespaceSpacing: the client reads envelope address lists and
// NAMESPACE lists in the grammar's form, with no SP between the items (as imapserver
// writes them since #377 and Dovecot always has), and in the spaced form older servers
// send.
func TestReadAddressListAndNamespaceSpacing(t *testing.T) {
	wantAddrs := []imap.Address{{Mailbox: "g1", Host: "d1.org"}, {Mailbox: "g2", Host: "d2.org"}}
	wantNS := []imap.NamespaceDescriptor{{Prefix: "", Delim: '/'}, {Prefix: "Shared/", Delim: '/'}}
	for _, sep := range []string{"", " "} {
		dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(
			`((NIL NIL "g1" "d1.org")`+sep+`(NIL NIL "g2" "d2.org"))`)), imapwire.ConnSideClient)
		addrs, err := readAddressList(dec, &Options{})
		if err != nil || !slices.Equal(addrs, wantAddrs) {
			t.Errorf("sep %q: readAddressList() = %+v, %v, want %+v", sep, addrs, err, wantAddrs)
		}

		dec = imapwire.NewDecoder(bufio.NewReader(strings.NewReader(
			`(("" "/")`+sep+`("Shared/" "/"))`)), imapwire.ConnSideClient)
		ns, err := readNamespace(dec)
		if err != nil || !slices.Equal(ns, wantNS) {
			t.Errorf("sep %q: readNamespace() = %+v, %v, want %+v", sep, ns, err, wantNS)
		}
	}
}
