package imapclient

import (
	"bufio"
	"slices"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// TestReadAddressKeepsSourceRoute: the client keeps the addr-adl of an address (the
// source route, RFC 9051 §7.5.2) instead of discarding it (#379).
func TestReadAddressKeepsSourceRoute(t *testing.T) {
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(
		`((NIL "@first,@second" "user" "domain")(NIL NIL "plain" "domain"))`)), imapwire.ConnSideClient)
	addrs, err := readAddressList(dec, &Options{})
	if err != nil {
		t.Fatalf("readAddressList(): %v", err)
	}
	want := []imap.Address{{Route: "@first,@second", Mailbox: "user", Host: "domain"}, {Mailbox: "plain", Host: "domain"}}
	if !slices.Equal(addrs, want) {
		t.Errorf("readAddressList() = %+v, want %+v", addrs, want)
	}
	// The route is not part of the address a client replies to.
	if got := addrs[0].Addr(); got != "user@domain" {
		t.Errorf("Addr() = %q, want user@domain", got)
	}
}
