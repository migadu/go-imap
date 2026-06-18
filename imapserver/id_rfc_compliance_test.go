package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestIDCommandRFCCompliance tests all valid ID command formats per RFC 2971
//
// RFC 2971 ABNF:
//
//	id_params_list ::= "(" #(string SPACE nstring) ")" / nil
//
// Where:
//   - NIL is a valid value
//   - () empty list is valid (# means 0 or more)
//   - Each pair is: string SPACE nstring
//   - nstring can be a string or NIL
func TestIDCommandRFCCompliance(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectNil   bool
		expectError bool
		description string
	}{
		{
			name:        "RFC Example 1: NIL",
			input:       " NIL\r\n",
			expectNil:   true,
			description: "Client sends NIL (wants no info sent, but will accept server response)",
		},
		{
			name:        "Lenient: bare ID with no argument",
			input:       "\r\n",
			expectNil:   true,
			description: "Open-Xchange sends bare \"ID\" instead of \"ID NIL\"; treat as NIL (RFC 2971 violation tolerated)",
		},
		{
			name:        "Lenient: bare ID with trailing space",
			input:       " \r\n",
			expectNil:   true,
			description: "Bare \"ID \" with trailing space and no argument; treat as NIL",
		},
		{
			name:        "RFC Example 2: Empty list",
			input:       " ()\r\n",
			description: "Zero field-value pairs (valid per # operator)",
		},
		{
			name:        "RFC Example 3: Single pair",
			input:       ` ("name" "sodr")` + "\r\n",
			description: "One field-value pair",
		},
		{
			name:        "RFC Example 4: Multiple pairs from spec",
			input:       ` ("name" "sodr" "version" "19.34" "vendor" "Pink Floyd Music Limited")` + "\r\n",
			description: "Multiple field-value pairs (from RFC 2971 example)",
		},
		{
			name:        "RFC Example 5: With NIL values",
			input:       ` ("name" "Cyrus" "version" "1.5" "os" "sunos" "os-version" "5.5" "support-url" "mailto:cyrus-bugs+@andrew.cmu.edu")` + "\r\n",
			description: "Complex example from RFC (Cyrus server response format)",
		},
		{
			name:        "RFC Compliant: NIL value in pair",
			input:       ` ("name" "client" "vendor" NIL)` + "\r\n",
			description: "nstring allows NIL as a value",
		},
		{
			name:        "RFC Compliant: Multiple NIL values",
			input:       ` ("name" "client" "version" NIL "vendor" NIL)` + "\r\n",
			description: "Multiple NIL values are allowed",
		},
		{
			name:        "RFC Compliant: All standard fields",
			input:       ` ("name" "test" "version" "1.0" "os" "linux" "os-version" "5.10" "vendor" "TestCorp" "support-url" "https://test.com" "address" "addr" "date" "2024-01-01" "command" "cmd" "arguments" "args" "environment" "env")` + "\r\n",
			description: "All standard fields from RFC 2971",
		},
		{
			name:        "RFC Limit: Exactly 30 pairs",
			input:       ` ("k1" "v1" "k2" "v2" "k3" "v3" "k4" "v4" "k5" "v5" "k6" "v6" "k7" "v7" "k8" "v8" "k9" "v9" "k10" "v10" "k11" "v11" "k12" "v12" "k13" "v13" "k14" "v14" "k15" "v15" "k16" "v16" "k17" "v17" "k18" "v18" "k19" "v19" "k20" "v20" "k21" "v21" "k22" "v22" "k23" "v23" "k24" "v24" "k25" "v25" "k26" "v26" "k27" "v27" "k28" "v28" "k29" "v29" "k30" "v30")` + "\r\n",
			description: "Maximum 30 field-value pairs per RFC 2971",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, imapwire.ConnSideServer)

			idData, err := readID(dec)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got success")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if tt.expectNil {
				if idData != nil {
					t.Errorf("Expected nil data, got: %+v", idData)
				}
			} else {
				if idData == nil {
					t.Error("Expected non-nil data")
				}
			}

			// Verify CRLF can be read
			if !dec.ExpectCRLF() {
				t.Errorf("Failed to read CRLF: %v", dec.Err())
			}

			// Verify no stale decoder error
			if dec.Err() != nil {
				t.Errorf("Decoder has stale error: %v", dec.Err())
			}

			t.Logf("✓ %s", tt.description)
		})
	}
}

// TestIDCommandRFC2971Violations tests invalid formats that violate RFC 2971
func TestIDCommandRFC2971Violations(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		violation string
	}{
		{
			name:      "Odd number of elements",
			input:     ` ("name" "test" "version")` + "\r\n",
			violation: "Missing value for key (not a complete pair)",
		},
		{
			name:      "Non-string key",
			input:     ` (name "test")` + "\r\n",
			violation: "Key must be a string, not an atom",
		},
		{
			name:      "Non-string/non-NIL value",
			input:     ` ("name" test)` + "\r\n",
			violation: "Value must be string or NIL, not an atom",
		},
		{
			name:      "Garbage after valid list",
			input:     ` ("name" "test")GARBAGE` + "\r\n",
			violation: "Extra data after id_params_list",
		},
		{
			name:      "Garbage after NIL",
			input:     ` NILGARBAGE` + "\r\n",
			violation: "Extra data after NIL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, imapwire.ConnSideServer)

			idData, readErr := readID(dec)

			var finalErr error
			if readErr != nil {
				finalErr = readErr
			} else {
				// Check CRLF
				if !dec.ExpectCRLF() {
					finalErr = dec.Err()
				}
			}

			if finalErr == nil {
				t.Errorf("RFC violation not detected: %s\nGot success with: %+v", tt.violation, idData)
				return
			}

			t.Logf("✓ Correctly rejected: %s\n  Error: %v", tt.violation, finalErr)
		})
	}
}

// TestIDCommandEdgeCases tests edge cases not explicitly covered by RFC
func TestIDCommandEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		shouldWork  bool
		description string
	}{
		{
			name:        "Quoted strings with spaces",
			input:       ` ("name" "My Client Application" "version" "1.0 beta")` + "\r\n",
			shouldWork:  true,
			description: "Strings with spaces should work",
		},
		{
			name:        "Quoted strings with special chars",
			input:       ` ("name" "client-v2" "support-url" "mailto:support@example.com")` + "\r\n",
			shouldWork:  true,
			description: "Special characters in strings",
		},
		{
			name:        "Empty string values",
			input:       ` ("name" "")` + "\r\n",
			shouldWork:  true,
			description: "Empty strings are valid",
		},
		{
			name:        "Case sensitive field names",
			input:       ` ("Name" "test" "VERSION" "1.0")` + "\r\n",
			shouldWork:  true,
			description: "Field names should be case-insensitive per IMAP convention",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, imapwire.ConnSideServer)

			idData, err := readID(dec)

			if tt.shouldWork {
				if err != nil {
					t.Fatalf("Should work but got error: %v", err)
				}

				if !dec.ExpectCRLF() {
					t.Fatalf("Failed to read CRLF: %v", dec.Err())
				}

				t.Logf("✓ %s: %+v", tt.description, idData)
			} else {
				if err == nil && dec.ExpectCRLF() {
					t.Errorf("Should fail but succeeded: %+v", idData)
				} else {
					t.Logf("✓ Correctly failed: %s", tt.description)
				}
			}
		})
	}
}
