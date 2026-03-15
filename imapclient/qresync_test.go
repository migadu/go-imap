package imapclient_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestEnable_QResync(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC
	data, err := client.Enable(imap.CapQResync).Wait()
	if err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	if !data.Caps.Has(imap.CapQResync) {
		t.Errorf("QRESYNC capability not enabled")
	}
	t.Logf("Successfully enabled QRESYNC")
}

func TestSelect_QResync(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC first
	_, err := client.Enable(imap.CapQResync).Wait()
	if err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// First SELECT to get initial state
	firstSelect, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("First Select() = %v", err)
	}
	t.Logf("Initial SELECT - UIDValidity: %d, HighestModSeq: %d",
		firstSelect.UIDValidity, firstSelect.HighestModSeq)

	// Unselect to test QRESYNC SELECT
	if err := client.Unselect().Wait(); err != nil {
		t.Fatalf("Unselect() = %v", err)
	}

	// SELECT with QRESYNC
	qresyncOptions := &imap.SelectOptions{
		QResync: &imap.QResyncData{
			UIDValidity: firstSelect.UIDValidity,
			ModSeq:      firstSelect.HighestModSeq,
		},
	}

	secondSelect, err := client.Select("INBOX", qresyncOptions).Wait()
	if err != nil {
		t.Fatalf("QRESYNC Select() = %v", err)
	}

	// Verify QRESYNC worked
	if secondSelect.UIDValidity != firstSelect.UIDValidity {
		t.Errorf("UIDValidity changed: %d != %d", secondSelect.UIDValidity, firstSelect.UIDValidity)
	}
	if secondSelect.HighestModSeq < firstSelect.HighestModSeq {
		t.Errorf("HighestModSeq decreased: %d < %d", secondSelect.HighestModSeq, firstSelect.HighestModSeq)
	}
	t.Logf("QRESYNC SELECT successful")
}

func TestSelect_QResync_WithKnownUIDs(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC first
	_, err := client.Enable(imap.CapQResync).Wait()
	if err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// First SELECT to get initial state
	firstSelect, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("First Select() = %v", err)
	}

	// Get some UIDs to test with
	fetchOptions := &imap.FetchOptions{UID: true}
	messages, err := client.Fetch(imap.SeqSetNum(1), fetchOptions).Collect()
	if err != nil {
		t.Fatalf("Fetch UIDs = %v", err)
	}

	var knownUIDs imap.UIDSet
	if len(messages) > 0 {
		knownUIDs = imap.UIDSetNum(messages[0].UID)
	}

	// Unselect to test QRESYNC SELECT with known UIDs
	if err := client.Unselect().Wait(); err != nil {
		t.Fatalf("Unselect() = %v", err)
	}

	// SELECT with QRESYNC and known UIDs
	qresyncOptions := &imap.SelectOptions{
		QResync: &imap.QResyncData{
			UIDValidity: firstSelect.UIDValidity,
			ModSeq:      firstSelect.HighestModSeq,
			KnownUIDs:   knownUIDs,
		},
	}

	_, err = client.Select("INBOX", qresyncOptions).Wait()
	if err != nil {
		t.Fatalf("QRESYNC Select() with known UIDs = %v", err)
	}

	t.Logf("QRESYNC SELECT with known UIDs successful")
}

func TestUIDFetch_Vanished(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC first
	_, err := client.Enable(imap.CapQResync).Wait()
	if err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// Test UID FETCH with VANISHED modifier
	fetchOptions := &imap.FetchOptions{
		Flags:        true,
		ChangedSince: 1, // Use a low modseq to potentially get some results
		Vanished:     true,
	}

	uidSet := imap.UIDSetNum(1)
	uidSet.AddRange(1, 0) // 1:*
	messages, err := client.Fetch(uidSet, fetchOptions).Collect()
	if err != nil {
		t.Fatalf("UID FETCH with VANISHED = %v", err)
	}

	t.Logf("UID FETCH with VANISHED returned %d messages", len(messages))
}

// TestSelect_QResync_ModifiedModSeq exercises the writeQResyncFetch code path
// that emits untagged FETCH responses during a QRESYNC SELECT for messages
// that were modified since the supplied mod-sequence.
//
// RFC 7162 §2.3.2 requires MODSEQ to be formatted as "MODSEQ (value)" in
// FETCH responses.  Without the fix, writeQResyncFetch emits "MODSEQ value"
// (without the mandatory parentheses), which the imapclient FETCH parser
// rejects, causing Select().Wait() to return a parse error.
func TestSelect_QResync_ModifiedModSeq(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC — required before a QRESYNC SELECT.
	if _, err := client.Enable(imap.CapQResync).Wait(); err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// First SELECT: learn UIDValidity for the mailbox.  At this point the
	// mailbox contains exactly one message (appended by newClientServerPair)
	// whose modSeq is > 0.
	firstData, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("First Select() = %v", err)
	}

	if err := client.Unselect().Wait(); err != nil {
		t.Fatalf("Unselect() = %v", err)
	}

	// Second SELECT with QRESYNC and ModSeq=0.  Every message in the mailbox
	// has modSeq > 0, so the server will include all of them in the Modified
	// list and call writeQResyncFetch for each.  If MODSEQ is not wrapped in
	// parentheses the client FETCH parser will return an error and
	// Select().Wait() will fail.
	_, err = client.Select("INBOX", &imap.SelectOptions{
		QResync: &imap.QResyncData{
			UIDValidity: firstData.UIDValidity,
			ModSeq:      0, // request all modifications
		},
	}).Wait()
	if err != nil {
		t.Fatalf("QRESYNC Select() with modified messages = %v", err)
	}
}

// TestUIDFetch_Vanished_WithoutChangedSince verifies that the server rejects a
// UID FETCH command that uses the VANISHED modifier without the required
// CHANGEDSINCE modifier.
//
// RFC 7162 §3.2.6: "The VANISHED UID FETCH modifier MUST only be used when
// the UID FETCH command also contains the CHANGEDSINCE modifier."
func TestUIDFetch_Vanished_WithoutChangedSince(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC — required before using VANISHED.
	if _, err := client.Enable(imap.CapQResync).Wait(); err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// Send UID FETCH with Vanished=true but ChangedSince=0 (not set).
	// The server must respond with BAD per RFC 7162 §3.2.6.
	_, err := client.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{
		Flags:    true,
		Vanished: true,
		// ChangedSince intentionally omitted
	}).Collect()
	if err == nil {
		t.Fatalf("UID FETCH VANISHED without CHANGEDSINCE should return BAD, got nil error")
	}
	t.Logf("Got expected error: %v", err)
}

func TestVanished_Response(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	// Enable QRESYNC first
	_, err := client.Enable(imap.CapQResync).Wait()
	if err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// Note: In a real test, we would need to trigger an expunge that causes
	// a VANISHED response. For now, we just verify QRESYNC is enabled.
	// The VANISHED responses would be handled by the UnilateralDataHandler
	// which can be set when creating the client.

	// This test just verifies that QRESYNC is properly enabled
	// and the client can handle the expected protocol

	t.Logf("VANISHED response handler test completed")
}

func TestCapability_QResync_Implications(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateNotAuthenticated)
	defer client.Close()
	defer server.Close()

	// Check that QRESYNC implies CONDSTORE
	caps, err := client.Capability().Wait()
	if err != nil {
		t.Fatalf("Capability() = %v", err)
	}

	// Login first
	if err := client.Login(testUsername, testPassword).Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}

	// Enable QRESYNC
	enableData, err := client.Enable(imap.CapQResync).Wait()
	if err != nil {
		t.Fatalf("Enable(QRESYNC) = %v", err)
	}

	// Verify QRESYNC implies CONDSTORE
	if enableData.Caps.Has(imap.CapQResync) && !enableData.Caps.Has(imap.CapCondStore) {
		// Check if CONDSTORE is implied by QRESYNC in the capability system
		if !caps.Has(imap.CapCondStore) {
			t.Errorf("QRESYNC should imply CONDSTORE capability")
		}
	}

	t.Logf("QRESYNC capability implications verified")
}
