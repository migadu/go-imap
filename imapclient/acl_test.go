package imapclient_test

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// order matters
var testCases = []struct {
	name                  string
	mailbox               string
	setRightsModification imap.RightModification
	setRights             imap.RightSet
	expectedRights        imap.RightSet
	execStatusCmd         bool
}{
	{
		name:                  "inbox",
		mailbox:               "INBOX",
		setRightsModification: imap.RightModificationReplace,
		setRights:             imap.RightSet("akxeilprwtscd"),
		expectedRights:        imap.RightSet("akxeilprwtscd"),
	},
	{
		name:                  "custom_folder",
		mailbox:               "MyFolder",
		setRightsModification: imap.RightModificationReplace,
		setRights:             imap.RightSet("ailw"),
		expectedRights:        imap.RightSet("ailw"),
	},
	{
		name:                  "custom_child_folder",
		mailbox:               "MyFolder/Child",
		setRightsModification: imap.RightModificationReplace,
		setRights:             imap.RightSet("aelrwtd"),
		expectedRights:        imap.RightSet("aelrwtd"),
	},
	{
		name:                  "add_rights",
		mailbox:               "MyFolder",
		setRightsModification: imap.RightModificationAdd,
		setRights:             imap.RightSet("rwi"),
		expectedRights:        imap.RightSet("ailwr"),
	},
	{
		name:                  "remove_rights",
		mailbox:               "MyFolder",
		setRightsModification: imap.RightModificationRemove,
		setRights:             imap.RightSet("iwc"),
		expectedRights:        imap.RightSet("alr"),
	},
	{
		name:                  "empty_rights",
		mailbox:               "MyFolder/Child",
		setRightsModification: imap.RightModificationReplace,
		setRights:             imap.RightSet("a"),
		expectedRights:        imap.RightSet("a"),
	},
}

// TestACL runs tests on SetACL, GetACL and MyRights commands.
func TestACL(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapACL) {
		t.Skipf("server doesn't support ACL")
	}

	if err := client.Create("MyFolder", nil).Wait(); err != nil {
		t.Fatalf("create MyFolder error: %v", err)
	}

	if err := client.Create("MyFolder/Child", nil).Wait(); err != nil {
		t.Fatalf("create MyFolder/Child error: %v", err)
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// execute SETACL command
			err := client.SetACL(tc.mailbox, testUsername, tc.setRightsModification, tc.setRights).Wait()
			if err != nil {
				t.Fatalf("SetACL().Wait() error: %v", err)
			}

			// execute GETACL command to reset cache on server
			getACLData, err := client.GetACL(tc.mailbox).Wait()
			if err != nil {
				t.Fatalf("GetACL().Wait() error: %v", err)
			}

			if !tc.expectedRights.Equal(getACLData.Rights[testUsername]) {
				t.Errorf("GETACL returned wrong rights; expected: %s, got: %s", tc.expectedRights, getACLData.Rights[testUsername])
			}

			// execute MYRIGHTS command
			myRightsData, err := client.MyRights(tc.mailbox).Wait()
			if err != nil {
				t.Errorf("MyRights().Wait() error: %v", err)
			}

			if !tc.expectedRights.Equal(myRightsData.Rights) {
				t.Errorf("MYRIGHTS returned wrong rights; expected: %s, got: %s", tc.expectedRights, myRightsData.Rights)
			}
		})
	}

	t.Run("nonexistent_mailbox", func(t *testing.T) {
		if client.SetACL("BibiMailbox", testUsername, imap.RightModificationReplace, nil).Wait() == nil {
			t.Errorf("expected error")
		}
	})
}

// TestDeleteACL tests the DELETEACL command
func TestDeleteACL(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapACL) {
		t.Skipf("server doesn't support ACL")
	}

	mailbox := "INBOX"
	identifier := imap.RightsIdentifier("testuser2")

	// First, set some rights
	err := client.SetACL(mailbox, identifier, imap.RightModificationReplace, imap.RightSet("lr")).Wait()
	if err != nil {
		t.Fatalf("SetACL().Wait() error: %v", err)
	}

	// Verify rights were set
	getACLData, err := client.GetACL(mailbox).Wait()
	if err != nil {
		t.Fatalf("GetACL().Wait() error: %v", err)
	}

	if _, ok := getACLData.Rights[identifier]; !ok {
		t.Fatalf("Rights not set for identifier %s", identifier)
	}

	// Delete the ACL entry
	err = client.DeleteACL(mailbox, identifier).Wait()
	if err != nil {
		t.Fatalf("DeleteACL().Wait() error: %v", err)
	}

	// Verify rights were deleted
	getACLData, err = client.GetACL(mailbox).Wait()
	if err != nil {
		t.Fatalf("GetACL().Wait() error: %v", err)
	}

	if rights, ok := getACLData.Rights[identifier]; ok && len(rights) > 0 {
		t.Errorf("Rights still exist for identifier %s after DeleteACL: %s", identifier, rights)
	}

	// Test deleting non-existent ACL (should not error)
	err = client.DeleteACL(mailbox, imap.RightsIdentifier("nonexistent")).Wait()
	if err != nil {
		t.Errorf("DeleteACL() for non-existent identifier returned error: %v", err)
	}

	// Test with non-existent mailbox
	err = client.DeleteACL("NonExistentMailbox", identifier).Wait()
	if err == nil {
		t.Errorf("DeleteACL() for non-existent mailbox should return error")
	}
}

// TestListRights tests the LISTRIGHTS command
func TestListRights(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapACL) {
		t.Skipf("server doesn't support ACL")
	}

	mailbox := "INBOX"
	identifier := imap.RightsIdentifier(testUsername)

	// Execute LISTRIGHTS command
	listRightsData, err := client.ListRights(mailbox, identifier).Wait()
	if err != nil {
		t.Fatalf("ListRights().Wait() error: %v", err)
	}

	// Verify we got data back
	if listRightsData.Mailbox != mailbox {
		t.Errorf("ListRights returned wrong mailbox: expected %s, got %s", mailbox, listRightsData.Mailbox)
	}

	if listRightsData.Identifier != identifier {
		t.Errorf("ListRights returned wrong identifier: expected %s, got %s", identifier, listRightsData.Identifier)
	}

	// RequiredRights is usually empty, but OptionalRights should have some rights
	if len(listRightsData.OptionalRights) == 0 {
		t.Errorf("ListRights returned no optional rights")
	}

	// Test with non-existent mailbox - some servers like Dovecot may not return an error
	_, err = client.ListRights("NonExistentMailbox", imap.RightsIdentifier("nonexistent")).Wait()
	if err != nil {
		t.Logf("ListRights() for non-existent mailbox returned error (as expected): %v", err)
	}
}

// TestACLMultipleIdentifiers tests ACL with multiple identifiers
func TestACLMultipleIdentifiers(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapACL) {
		t.Skipf("server doesn't support ACL")
	}

	if err := client.Create("SharedFolder", nil).Wait(); err != nil {
		t.Fatalf("create SharedFolder error: %v", err)
	}

	mailbox := "SharedFolder"
	identifiers := []imap.RightsIdentifier{
		imap.RightsIdentifier(testUsername),
		imap.RightsIdentifier("user2@example.com"),
		imap.RightsIdentifier("user3@example.com"),
	}

	// Set rights for multiple identifiers
	for i, identifier := range identifiers {
		rights := imap.RightSet("lr")
		if i == 0 {
			rights = imap.RightSet("lrswipkxtea") // Full rights for owner
		}

		err := client.SetACL(mailbox, identifier, imap.RightModificationReplace, rights).Wait()
		if err != nil {
			t.Fatalf("SetACL() for %s error: %v", identifier, err)
		}
	}

	// Test 'anyone' identifier separately as some servers (like Dovecot) may disallow it
	err := client.SetACL(mailbox, imap.RightsIdentifierAnyone, imap.RightModificationReplace, imap.RightSet("lr")).Wait()
	if err != nil {
		t.Logf("SetACL() for 'anyone' returned error (some servers disallow it): %v", err)
	} else {
		// If it succeeded, add it to our identifiers list for verification
		identifiers = append(identifiers, imap.RightsIdentifierAnyone)
	}

	// Get and verify all ACLs
	getACLData, err := client.GetACL(mailbox).Wait()
	if err != nil {
		t.Fatalf("GetACL().Wait() error: %v", err)
	}

	if len(getACLData.Rights) != len(identifiers) {
		t.Errorf("Expected %d ACL entries, got %d", len(identifiers), len(getACLData.Rights))
	}

	for _, identifier := range identifiers {
		if _, ok := getACLData.Rights[identifier]; !ok {
			t.Errorf("Missing ACL entry for identifier %s", identifier)
		}
	}

	// Test modifying specific identifier
	err = client.SetACL(mailbox, identifiers[1], imap.RightModificationAdd, imap.RightSet("w")).Wait()
	if err != nil {
		t.Fatalf("SetACL() add rights error: %v", err)
	}

	getACLData, err = client.GetACL(mailbox).Wait()
	if err != nil {
		t.Fatalf("GetACL().Wait() error: %v", err)
	}

	expectedRights := imap.RightSet("lrw")
	if !expectedRights.Equal(getACLData.Rights[identifiers[1]]) {
		t.Errorf("Rights after add: expected %s, got %s", expectedRights, getACLData.Rights[identifiers[1]])
	}
}

// TestACLRFC4314Rights tests RFC 4314 specific rights
func TestACLRFC4314Rights(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapACL) {
		t.Skipf("server doesn't support ACL")
	}

	mailbox := "INBOX"
	identifier := imap.RightsIdentifier(testUsername)

	// Test new RFC 4314 rights: k, x, t, e (include 'a' to maintain admin rights)
	newRights := imap.RightSet("kxtea")
	err := client.SetACL(mailbox, identifier, imap.RightModificationReplace, newRights).Wait()
	if err != nil {
		t.Fatalf("SetACL() with RFC 4314 rights error: %v", err)
	}

	getACLData, err := client.GetACL(mailbox).Wait()
	if err != nil {
		t.Fatalf("GetACL().Wait() error: %v", err)
	}

	// Some servers (like Dovecot) automatically map obsolete rights c/d when setting new rights
	// So we check that at minimum the requested rights are present
	gotRights := getACLData.Rights[identifier]
	for _, r := range string(newRights) {
		if !strings.ContainsRune(string(gotRights), rune(r)) {
			t.Errorf("RFC 4314 rights: expected to have right %c in %s", r, gotRights)
		}
	}

	// Test that obsolete rights (c, d) still work
	obsoleteRights := imap.RightSet("cd")
	err = client.SetACL(mailbox, identifier, imap.RightModificationAdd, obsoleteRights).Wait()
	if err != nil {
		t.Fatalf("SetACL() with obsolete rights error: %v", err)
	}

	myRightsData, err := client.MyRights(mailbox).Wait()
	if err != nil {
		t.Fatalf("MyRights().Wait() error: %v", err)
	}

	// Verify obsolete rights were added
	expectedWithObsolete := imap.RightSet("kxteacd")
	if !expectedWithObsolete.Equal(myRightsData.Rights) {
		t.Errorf("Rights with obsolete: expected %s, got %s", expectedWithObsolete, myRightsData.Rights)
	}
}
