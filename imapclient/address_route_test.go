package imapclient

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/migadu/go-imap/internal/imapwire"
)

// TestReadAddressKeepsSourceRoute: the client keeps the addr-adl of an address (the
// source route, RFC 9051 §7.5.2) instead of discarding it (#379).
func TestReadAddressKeepsSourceRoute(t *testing.T) {
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(
		`((NIL "@first,@second" "user" "domain")(NIL NIL "plain" "domain"))`)), imapwire.ConnSideClient)
	addrs, err := readAddressList(dec, &Options{})
	if err != nil || len(addrs) != 2 {
		t.Fatalf("readAddressList() = %+v, %v", addrs, err)
	}
	if got := fmt.Sprintf("%+v", addrs[0]); !strings.Contains(got, "@first,@second") {
		t.Errorf("first address is %s, want it to keep the route @first,@second", got)
	}
	if got := fmt.Sprintf("%+v", addrs[1]); strings.Contains(got, "@") {
		t.Errorf("second address is %s, want no route", got)
	}
}
