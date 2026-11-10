package imapclient_test

import (
	"testing"

	"github.com/migadu/go-imap"
)

// RFC 4314 §6: rights-capa = "RIGHTS=" new-rights, new-rights = 1*LOWER-ALPHA.
// §2: "uppercase rights are not allowed". Capability names are
// case-insensitive, so the client canonicalizes them to upper case, but the
// rights listed by RIGHTS= are data and must come through as sent.
//
// Issue #145.
func TestCapabilityRightsKeepCase(t *testing.T) {
	for _, line := range []string{
		"* CAPABILITY IMAP4rev1 ACL RIGHTS=kxte\r\n",
		"* CAPABILITY imap4rev1 acl rights=kxte\r\n",
	} {
		client := newCodeServer(t, line)
		caps, err := client.Capability().Wait()
		if err != nil {
			t.Fatalf("Capability() = %v", err)
		}
		if !caps.Has(imap.Cap("RIGHTS=kxte")) {
			t.Errorf("%q: the capabilities are %v, want RIGHTS=kxte among them", line, caps)
		}
		if !caps.Has(imap.CapACL) || !caps.Has(imap.CapIMAP4rev1) {
			t.Errorf("%q: the capabilities are %v, want ACL and IMAP4rev1 canonicalized", line, caps)
		}
	}
}
