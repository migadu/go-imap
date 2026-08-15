package imapclient_test

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestFetchHeaderFieldsRoundTrip exercises BODY[HEADER.FIELDS (...)] end to end
// against our own server, so the unquoted header names the client now emits are
// proven to parse on the receiving side and to select the right headers.
//
// There was no end-to-end coverage of HEADER.FIELDS before, which is why the
// encoding could be changed without anything noticing.
func TestFetchHeaderFieldsRoundTrip(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	seqSet := imap.SeqSetNum(1)
	fetchOptions := &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"Message-Id", "Content-Type"},
		}},
	}

	messages, err := client.Fetch(seqSet, fetchOptions).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %v, want 1", len(messages))
	}

	var body string
	for _, buf := range messages[0].BodySection {
		body = string(buf.Bytes)
	}
	if body == "" {
		t.Fatal("no body section returned for HEADER.FIELDS")
	}

	// The requested headers must be there...
	for _, want := range []string{"Message-Id:", "Content-Type:"} {
		if !strings.Contains(body, want) {
			t.Errorf("body section %q does not contain %q", body, want)
		}
	}
	// ...and the unrequested ones must not, which is what proves the field
	// list was actually parsed rather than ignored.
	for _, notWant := range []string{"MIME-Version:", "Content-Transfer-Encoding:"} {
		if strings.Contains(body, notWant) {
			t.Errorf("body section %q contains unrequested header %q", body, notWant)
		}
	}
}

// TestFetchHeaderFieldsNotRoundTrip is the HEADER.FIELDS.NOT counterpart.
func TestFetchHeaderFieldsNotRoundTrip(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	fetchOptions := &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:       imap.PartSpecifierHeader,
			HeaderFieldsNot: []string{"Message-Id"},
		}},
	}

	messages, err := client.Fetch(imap.SeqSetNum(1), fetchOptions).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %v, want 1", len(messages))
	}

	var body string
	for _, buf := range messages[0].BodySection {
		body = string(buf.Bytes)
	}
	if strings.Contains(body, "Message-Id:") {
		t.Errorf("body section %q contains the excluded header Message-Id", body)
	}
	if !strings.Contains(body, "Content-Type:") {
		t.Errorf("body section %q does not contain Content-Type:", body)
	}
}
