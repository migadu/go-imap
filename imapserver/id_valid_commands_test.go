package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestValidIDCommands ensures our fix doesn't break valid ID commands
func TestValidIDCommands(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectNil   bool
		expectName  string
		expectError bool
	}{
		{
			name:        "NIL",
			input:       " NIL\r\n",
			expectNil:   true,
			expectError: false,
		},
		{
			name:        "Empty list",
			input:       " ()\r\n",
			expectNil:   false,
			expectError: false,
		},
		{
			name:        "Single key-value pair",
			input:       ` ("name" "TestClient")` + "\r\n",
			expectName:  "TestClient",
			expectError: false,
		},
		{
			name:        "Multiple pairs",
			input:       ` ("name" "TestClient" "version" "1.0" "os" "Linux")` + "\r\n",
			expectName:  "TestClient",
			expectError: false,
		},
		{
			name:        "With NIL values",
			input:       ` ("name" "TestClient" "vendor" NIL)` + "\r\n",
			expectName:  "TestClient",
			expectError: false,
		},
		{
			name:        "Apple Mail example",
			input:       ` ("name" "com.apple.email.maild" "version" "3864.100.1.2.9" "os" "iOS" "os-version" "26.0.1 (23A355)" "vendor" "Apple Inc" "event" NIL)` + "\r\n",
			expectName:  "com.apple.email.maild",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, imapwire.ConnSideServer)

			// Call readID
			idData, err := readID(dec)

			// Check error expectation
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Check NIL expectation
			if tt.expectNil {
				if idData != nil {
					t.Errorf("Expected nil data, got: %+v", idData)
				}
				return
			}

			// Check data is not nil for non-NIL cases
			if idData == nil {
				t.Fatal("Expected non-nil data")
			}

			// Check name if specified
			if tt.expectName != "" && idData.Name != tt.expectName {
				t.Errorf("Expected name %q, got %q", tt.expectName, idData.Name)
			}

			// Verify CRLF can be read successfully
			if !dec.ExpectCRLF() {
				t.Errorf("Failed to read CRLF: %v", dec.Err())
			}

			// Verify no stale decoder error
			if dec.Err() != nil {
				t.Errorf("Decoder has stale error after successful parse: %v", dec.Err())
			}

			t.Logf("✓ Successfully parsed: %+v", idData)
		})
	}
}

// TestInvalidIDCommands ensures bad commands are properly rejected
func TestInvalidIDCommands(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		errorContains string
	}{
		{
			name:          "Orphaned key",
			input:         ` ("name")` + "\r\n",
			errorContains: "odd number",
		},
		{
			name:          "Invalid value token",
			input:         ` ("name" =invalid)` + "\r\n",
			errorContains: "nstring",
		},
		{
			name:          "Atom instead of string for key",
			input:         ` (name "value")` + "\r\n",
			errorContains: "expected string key",
		},
		{
			name:          "Extra data after list",
			input:         ` ()=garbage` + "\r\n",
			errorContains: "expected CRLF",
		},
		{
			name:          "Extra data after valid list",
			input:         ` ("name" "test")=extra` + "\r\n",
			errorContains: "expected CRLF",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, imapwire.ConnSideServer)

			// Call readID
			idData, readErr := readID(dec)

			// Some errors happen in readID, others when trying to read CRLF
			var finalErr error
			if readErr != nil {
				finalErr = readErr
			} else {
				// readID succeeded, but there might be garbage after
				if !dec.ExpectCRLF() {
					finalErr = dec.Err()
				}
			}

			if finalErr == nil {
				t.Errorf("Expected error containing %q, but got success with data: %+v", tt.errorContains, idData)
				return
			}

			if !strings.Contains(finalErr.Error(), tt.errorContains) {
				t.Errorf("Expected error containing %q, got: %v", tt.errorContains, finalErr)
			}

			t.Logf("✓ Correctly rejected with error: %v", finalErr)
		})
	}
}
