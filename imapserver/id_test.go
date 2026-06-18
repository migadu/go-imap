package imapserver

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadID tests the readID function with various valid and invalid inputs
func TestReadID(t *testing.T) {
	tests := []struct {
		name        string
		input       string // IMAP wire format after "ID"
		wantErr     bool
		errContains string
		wantName    string
		wantVersion string
		wantOS      string
		wantVendor  string
	}{
		{
			name:    "valid NIL",
			input:   " NIL\r\n",
			wantErr: false,
			// NIL returns nil data, will be checked separately
		},
		{
			name:        "valid empty list",
			input:       " ()\r\n",
			wantErr:     false,
			wantName:    "",
			wantVersion: "",
		},
		{
			name:        "valid single pair",
			input:       ` ("name" "test")` + "\r\n",
			wantErr:     false,
			wantName:    "test",
			wantVersion: "",
		},
		{
			name:        "valid two pairs",
			input:       ` ("name" "test" "version" "1.0")` + "\r\n",
			wantErr:     false,
			wantName:    "test",
			wantVersion: "1.0",
		},
		{
			name:        "valid with NIL value",
			input:       ` ("name" "test" "vendor" NIL)` + "\r\n",
			wantErr:     false,
			wantName:    "test",
			wantVersion: "",
		},
		{
			name:        "Apple Mail example with NIL",
			input:       ` ("name" "com.apple.email.maild" "version" "3864.100.1.2.9" "os" "iOS" "os-version" "26.0.1 (23A355)" "vendor" "Apple Inc" "event" NIL)` + "\r\n",
			wantErr:     false,
			wantName:    "com.apple.email.maild",
			wantVersion: "3864.100.1.2.9",
			wantOS:      "iOS",
			wantVendor:  "Apple Inc",
		},
		{
			name:        "odd number - single key",
			input:       ` ("name")` + "\r\n",
			wantErr:     true,
			errContains: "odd number",
		},
		{
			name:        "odd number - three items",
			input:       ` ("name" "test" "version")` + "\r\n",
			wantErr:     true,
			errContains: `odd number of parameters, missing value for key "version" (received 3 parameters: [name test version])`,
		},
		{
			name:        "odd number - five items",
			input:       ` ("name" "test" "version" "1.0" "os")` + "\r\n",
			wantErr:     true,
			errContains: "odd number",
		},
		{
			name:        "malformed - non-string in list",
			input:       ` ("name" "test" JUNK)` + "\r\n",
			wantErr:     true,
			errContains: "expected string key",
		},
		{
			name:        "malformed - atom instead of string",
			input:       ` (name test)` + "\r\n",
			wantErr:     true,
			errContains: "expected string key",
		},
		{
			name:        "odd number with pipelined command simulation",
			input:       ` ("name" "test" "version")FETCH 1 ALL` + "\r\n",
			wantErr:     true,
			errContains: "missing value for key",
		},
		{
			name:        "invalid atom as value - equals sign",
			input:       ` ("name" =something)` + "\r\n",
			wantErr:     true,
			errContains: "in id key-val list",
		},
		{
			name:        "invalid atom as value in middle",
			input:       ` ("name" "test" "version" =1.0)` + "\r\n",
			wantErr:     true,
			errContains: "in id key-val list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create decoder from input
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tt.input)), 0)

			// Call readID
			data, err := readID(dec)

			// Check error expectations
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error containing %q, got no error", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Expected error containing %q, got %q", tt.errContains, err.Error())
				} else {
					// Log the error message for verification
					t.Logf("Error message: %s", err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// For NIL input, data can be nil
			if tt.name == "valid NIL" {
				if data != nil {
					t.Errorf("Expected nil data for NIL, got %v", data)
				}
				return
			}

			// Verify data for non-NIL cases
			if data == nil {
				t.Fatal("Expected non-nil data")
			}

			if data.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", data.Name, tt.wantName)
			}

			if data.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", data.Version, tt.wantVersion)
			}

			if tt.wantOS != "" && data.OS != tt.wantOS {
				t.Errorf("OS = %q, want %q", data.OS, tt.wantOS)
			}

			if tt.wantVendor != "" && data.Vendor != tt.wantVendor {
				t.Errorf("Vendor = %q, want %q", data.Vendor, tt.wantVendor)
			}

			// For Apple Mail test, verify NIL value for "event" is stored as empty string
			if tt.name == "Apple Mail example with NIL" {
				if eventValue, ok := data.Raw["event"]; !ok {
					t.Error("Expected 'event' key in Raw map")
				} else if eventValue != "" {
					t.Errorf("Expected 'event' value to be empty string for NIL, got %q", eventValue)
				}
			}
		})
	}
}

// TestReadID_NoUnparsedData verifies that when readID encounters an error,
// it doesn't leave unparsed data in the decoder that would cause subsequent
// parsing failures (like "expected CRLF, got '='")
func TestReadID_NoUnparsedData(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantErr     bool
		errContains string
	}{
		{
			name:        "invalid atom with equals - should not leave data",
			input:       ` ("name" =something)` + "\r\n",
			wantErr:     true,
			errContains: "in id key-val list",
		},
		{
			name:        "invalid atom in middle - should not leave data",
			input:       ` ("name" "test" "version" =1.0)` + "\r\n",
			wantErr:     true,
			errContains: "in id key-val list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a reader with the full input including CRLF
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, 0)

			// Call readID - this should consume all invalid tokens
			data, err := readID(dec)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Unexpected error: %v", err)
				}
				if data == nil {
					t.Fatal("Expected non-nil data")
				}
				return
			}

			// Verify we got the expected error
			if err == nil {
				t.Fatalf("Expected error containing %q, got no error", tt.errContains)
			}
			if !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("Expected error containing %q, got %q", tt.errContains, err.Error())
			}

			// The decoder should be in an error state, and the error should be the same
			if decErr := dec.Err(); decErr == nil {
				t.Error("Expected decoder to have an error set")
			}

			// Verify that we don't have "expected CRLF" errors when trying to read CRLF
			// This simulates what handleID does after calling readID
			if !dec.ExpectCRLF() {
				err := dec.Err()
				if err != nil && strings.Contains(err.Error(), "expected CRLF") {
					// Check what character was left unparsed
					remaining, _ := io.ReadAll(reader)
					t.Errorf("After readID error, ExpectCRLF failed with CRLF error: %v. Remaining unparsed: %q", err, remaining)
				}
			}
		})
	}
}
