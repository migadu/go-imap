package imapclient_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestMetadata_ServerAnnotations(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set server annotation
	entries := map[string]*[]byte{
		"/private/comment": ptr([]byte("my server comment")),
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Get server annotation
	data, err := client.GetMetadata("", []string{"/private/comment"}, nil).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	if data.Mailbox != "" {
		t.Errorf("GetMetadata().Mailbox = %q, want empty string", data.Mailbox)
	}

	if len(data.Entries) != 1 {
		t.Fatalf("GetMetadata().Entries length = %d, want 1", len(data.Entries))
	}

	value := data.Entries["/private/comment"]
	if value == nil {
		t.Fatal("GetMetadata().Entries['/private/comment'] = nil")
	}
	if string(*value) != "my server comment" {
		t.Errorf("GetMetadata().Entries['/private/comment'] = %q, want %q", string(*value), "my server comment")
	}
}

func TestMetadata_MailboxAnnotations(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set mailbox annotation
	entries := map[string]*[]byte{
		"/private/comment": ptr([]byte("my mailbox comment")),
	}
	if err := client.SetMetadata("INBOX", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Get mailbox annotation
	data, err := client.GetMetadata("INBOX", []string{"/private/comment"}, nil).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	if data.Mailbox != "INBOX" {
		t.Errorf("GetMetadata().Mailbox = %q, want 'INBOX'", data.Mailbox)
	}

	if len(data.Entries) != 1 {
		t.Fatalf("GetMetadata().Entries length = %d, want 1", len(data.Entries))
	}

	value := data.Entries["/private/comment"]
	if value == nil {
		t.Fatal("GetMetadata().Entries['/private/comment'] = nil")
	}
	if string(*value) != "my mailbox comment" {
		t.Errorf("GetMetadata().Entries['/private/comment'] = %q, want %q", string(*value), "my mailbox comment")
	}
}

func TestMetadata_DeleteEntry(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set annotation
	entries := map[string]*[]byte{
		"/private/comment": ptr([]byte("test")),
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Delete annotation (set to nil)
	entries = map[string]*[]byte{
		"/private/comment": nil,
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() delete = %v", err)
	}

	// Verify it's deleted
	data, err := client.GetMetadata("", []string{"/private/comment"}, nil).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	if len(data.Entries) != 0 {
		t.Errorf("GetMetadata().Entries length = %d, want 0", len(data.Entries))
	}
}

func TestMetadata_DepthZero(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set multiple annotations
	entries := map[string]*[]byte{
		"/private/comment":       ptr([]byte("parent")),
		"/private/comment/child": ptr([]byte("child")),
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Get with depth 0 (exact match only)
	options := &imap.GetMetadataOptions{
		Depth: imap.GetMetadataDepthZero,
	}
	data, err := client.GetMetadata("", []string{"/private/comment"}, options).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	// Should only get exact match
	if len(data.Entries) != 1 {
		t.Fatalf("GetMetadata().Entries length = %d, want 1", len(data.Entries))
	}

	if _, ok := data.Entries["/private/comment"]; !ok {
		t.Error("Expected /private/comment in results")
	}
	if _, ok := data.Entries["/private/comment/child"]; ok {
		t.Error("Did not expect /private/comment/child in results with depth 0")
	}
}

func TestMetadata_DepthOne(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set multiple annotations
	entries := map[string]*[]byte{
		"/private/comment":             ptr([]byte("parent")),
		"/private/comment/child":       ptr([]byte("child")),
		"/private/comment/child/grand": ptr([]byte("grandchild")),
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Get with depth 1 (immediate children)
	options := &imap.GetMetadataOptions{
		Depth: imap.GetMetadataDepthOne,
	}
	data, err := client.GetMetadata("", []string{"/private/comment"}, options).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	// Should get parent and immediate child, but not grandchild
	if len(data.Entries) != 2 {
		t.Fatalf("GetMetadata().Entries length = %d, want 2", len(data.Entries))
	}

	if _, ok := data.Entries["/private/comment"]; !ok {
		t.Error("Expected /private/comment in results")
	}
	if _, ok := data.Entries["/private/comment/child"]; !ok {
		t.Error("Expected /private/comment/child in results")
	}
	if _, ok := data.Entries["/private/comment/child/grand"]; ok {
		t.Error("Did not expect /private/comment/child/grand in results with depth 1")
	}
}

func TestMetadata_DepthInfinity(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set multiple annotations
	entries := map[string]*[]byte{
		"/private/comment":             ptr([]byte("parent")),
		"/private/comment/child":       ptr([]byte("child")),
		"/private/comment/child/grand": ptr([]byte("grandchild")),
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Get with depth infinity (all descendants)
	options := &imap.GetMetadataOptions{
		Depth: imap.GetMetadataDepthInfinity,
	}
	data, err := client.GetMetadata("", []string{"/private/comment"}, options).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	// Should get all entries
	if len(data.Entries) != 3 {
		t.Fatalf("GetMetadata().Entries length = %d, want 3", len(data.Entries))
	}

	if _, ok := data.Entries["/private/comment"]; !ok {
		t.Error("Expected /private/comment in results")
	}
	if _, ok := data.Entries["/private/comment/child"]; !ok {
		t.Error("Expected /private/comment/child in results")
	}
	if _, ok := data.Entries["/private/comment/child/grand"]; !ok {
		t.Error("Expected /private/comment/child/grand in results")
	}
}

func TestMetadata_MaxSize(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set annotations with different sizes
	entries := map[string]*[]byte{
		"/private/small": ptr([]byte("small")),
		"/private/large": ptr([]byte("this is a much larger annotation value")),
	}
	if err := client.SetMetadata("", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Get with maxsize limit
	maxSize := uint32(10)
	options := &imap.GetMetadataOptions{
		MaxSize: &maxSize,
	}
	data, err := client.GetMetadata("", []string{"/private/small", "/private/large"}, options).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() = %v", err)
	}

	// Should only get small entry
	if len(data.Entries) != 1 {
		t.Fatalf("GetMetadata().Entries length = %d, want 1", len(data.Entries))
	}

	if _, ok := data.Entries["/private/small"]; !ok {
		t.Error("Expected /private/small in results")
	}
	if _, ok := data.Entries["/private/large"]; ok {
		t.Error("Did not expect /private/large in results (exceeds maxsize)")
	}
}

func TestSetMetadata_InvalidEntry(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	tests := []struct {
		name  string
		entry string
	}{
		{
			name:  "missing prefix",
			entry: "/invalid/entry",
		},
		{
			name:  "wrong prefix",
			entry: "/public/comment",
		},
		{
			name:  "contains wildcard asterisk",
			entry: "/private/comm*ent",
		},
		{
			name:  "contains wildcard percent",
			entry: "/private/comm%ent",
		},
		{
			name:  "consecutive slashes",
			entry: "/private//comment",
		},
		{
			name:  "trailing slash",
			entry: "/private/comment/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := map[string]*[]byte{
				tt.entry: ptr([]byte("test")),
			}
			err := client.SetMetadata("", entries).Wait()
			if err == nil {
				t.Fatal("Expected error for invalid entry, got nil")
			}
			// Verify the error mentions the invalid entry
			if !strings.Contains(err.Error(), "invalid entry name") {
				t.Errorf("Error should mention 'invalid entry name', got: %v", err)
			}
		})
	}
}

func TestGetMetadata_InvalidEntry(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	tests := []struct {
		name  string
		entry string
	}{
		{
			name:  "missing prefix",
			entry: "/invalid/entry",
		},
		{
			name:  "wrong prefix",
			entry: "/public/comment",
		},
		{
			name:  "contains wildcard asterisk",
			entry: "/private/comm*ent",
		},
		{
			name:  "contains wildcard percent",
			entry: "/private/comm%ent",
		},
		{
			name:  "consecutive slashes",
			entry: "/private//comment",
		},
		{
			name:  "trailing slash",
			entry: "/private/comment/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.GetMetadata("", []string{tt.entry}, nil).Wait()
			if err == nil {
				t.Fatal("Expected error for invalid entry, got nil")
			}
			// Verify the error mentions the invalid entry
			if !strings.Contains(err.Error(), "invalid entry name") {
				t.Errorf("Error should mention 'invalid entry name', got: %v", err)
			}
		})
	}
}

func TestMetadata_MailboxRename(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Create a test mailbox
	if err := client.Create("TestMailbox", nil).Wait(); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	// Set annotations on the mailbox
	entries := map[string]*[]byte{
		"/private/comment": ptr([]byte("my test comment")),
		"/shared/vendor":   ptr([]byte("vendor data")),
	}
	if err := client.SetMetadata("TestMailbox", entries).Wait(); err != nil {
		t.Fatalf("SetMetadata() = %v", err)
	}

	// Verify annotations exist
	data, err := client.GetMetadata("TestMailbox", []string{"/private/comment", "/shared/vendor"}, nil).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() before rename = %v", err)
	}
	if len(data.Entries) != 2 {
		t.Fatalf("Expected 2 entries before rename, got %d", len(data.Entries))
	}

	// Rename the mailbox
	if err := client.Rename("TestMailbox", "RenamedMailbox", nil).Wait(); err != nil {
		t.Fatalf("Rename() = %v", err)
	}

	// Verify annotations moved to the new mailbox name
	data, err = client.GetMetadata("RenamedMailbox", []string{"/private/comment", "/shared/vendor"}, nil).Wait()
	if err != nil {
		t.Fatalf("GetMetadata() after rename = %v", err)
	}

	if len(data.Entries) != 2 {
		t.Fatalf("Expected 2 entries after rename, got %d", len(data.Entries))
	}

	// Verify the annotation values are preserved
	if data.Entries["/private/comment"] == nil {
		t.Fatal("Expected /private/comment to exist after rename")
	}
	if string(*data.Entries["/private/comment"]) != "my test comment" {
		t.Errorf("Expected comment 'my test comment', got %q", string(*data.Entries["/private/comment"]))
	}

	if data.Entries["/shared/vendor"] == nil {
		t.Fatal("Expected /shared/vendor to exist after rename")
	}
	if string(*data.Entries["/shared/vendor"]) != "vendor data" {
		t.Errorf("Expected vendor 'vendor data', got %q", string(*data.Entries["/shared/vendor"]))
	}

	// Verify old mailbox name no longer has annotations
	_, err = client.GetMetadata("TestMailbox", []string{"/private/comment"}, nil).Wait()
	if err == nil {
		t.Error("Expected error when querying old mailbox name, got nil")
	}
}

func TestMetadata_TooManyResponseCode(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	// Set 101 entries to trigger TOOMANY (server limit is 100)
	entries := make(map[string]*[]byte)
	for i := 0; i < 101; i++ {
		entries[fmt.Sprintf("/private/entry%d", i)] = ptr([]byte("value"))
	}

	err := client.SetMetadata("", entries).Wait()
	if err == nil {
		t.Fatal("Expected TOOMANY error, got nil")
	}

	// Verify it's a TOOMANY response code
	if imapErr, ok := err.(*imap.Error); ok {
		if imapErr.Code != imap.ResponseCodeTooMany {
			t.Errorf("Expected TOOMANY response code, got %v", imapErr.Code)
		}
		if imapErr.Type != imap.StatusResponseTypeNo {
			t.Errorf("Expected NO response type, got %v", imapErr.Type)
		}
	} else {
		t.Errorf("Expected *imap.Error, got %T: %v", err, err)
	}
}

func TestMetadata_ResponseCodes(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapMetadata) {
		t.Skip("server doesn't support METADATA")
	}

	t.Run("TOOMANY on SetMetadata", func(t *testing.T) {
		// Set more than 100 entries to trigger TOOMANY
		entries := make(map[string]*[]byte)
		for i := 0; i < 101; i++ {
			entries[fmt.Sprintf("/private/test%d", i)] = ptr([]byte("value"))
		}

		err := client.SetMetadata("", entries).Wait()
		if err == nil {
			t.Fatal("Expected error, got nil")
		}

		imapErr, ok := err.(*imap.Error)
		if !ok {
			t.Fatalf("Expected *imap.Error, got %T", err)
		}

		if imapErr.Code != imap.ResponseCodeTooMany {
			t.Errorf("Expected TOOMANY, got %v", imapErr.Code)
		}
	})

	t.Run("MAXSIZE filtering", func(t *testing.T) {
		// Clear any previous entries first by creating a fresh client
		freshClient, freshServer := newClientServerPair(t, imap.ConnStateAuthenticated)
		defer freshClient.Close()
		defer freshServer.Close()

		// Set a large entry
		entries := map[string]*[]byte{
			"/private/large": ptr([]byte(strings.Repeat("x", 100))),
			"/private/small": ptr([]byte("small")),
		}
		if err := freshClient.SetMetadata("", entries).Wait(); err != nil {
			t.Fatalf("SetMetadata() = %v", err)
		}

		// Request with very small MAXSIZE
		maxSize := uint32(10)
		data, err := freshClient.GetMetadata("", []string{"/private/large", "/private/small"},
			&imap.GetMetadataOptions{MaxSize: &maxSize}).Wait()

		if err != nil {
			t.Fatalf("GetMetadata() = %v", err)
		}

		// Should only get small entry (large one filtered by MAXSIZE)
		if len(data.Entries) != 1 {
			t.Errorf("Expected 1 entry, got %d", len(data.Entries))
		}
		if _, ok := data.Entries["/private/small"]; !ok {
			t.Error("Expected /private/small in results")
		}
		if _, ok := data.Entries["/private/large"]; ok {
			t.Error("Did not expect /private/large (should be filtered by MAXSIZE)")
		}

		// Note: Current implementation does not return LONGENTRIES response code
		// This is a known limitation - the server silently filters large entries
		// A future enhancement would be to track and report LONGENTRIES
	})
}

func ptr(b []byte) *[]byte {
	return &b
}
