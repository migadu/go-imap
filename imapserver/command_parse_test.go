package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestCommandParsingEmptyLineHandler tests the empty line handler in readCommand
// This test verifies the fix for production error: "expected CRLF, got '='"
func TestCommandParsingEmptyLineHandler(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		errorContains string // What error we expect when parsing tag/command
	}{
		{
			name:          "Junk line starting with equals - equals is part of atom",
			input:         "=something\r\n",
			errorContains: "expected SP", // =something is consumed as tag, then expects SP
		},
		{
			name:          "Junk line starting with open paren - should get proper atom error",
			input:         "(something\r\n",
			errorContains: "expected atom, got \"(\"", // Proper error, not CRLF error
		},
		{
			name:          "Valid command after empty line",
			input:         "\r\nA001 NOOP\r\n",
			errorContains: "", // Should succeed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tt.input))
			dec := imapwire.NewDecoder(reader, imapwire.ConnSideServer)

			// Simulate the empty line handler in readCommand
			// This is what happens at conn.go:215 (using CRLF() not ExpectCRLF())
			if dec.CRLF() {
				t.Log("CRLF succeeded (empty line skipped)")
				// In real code, this would continue the loop
				// For valid commands after empty line, test passes
				return
			}

			// CRLF() failed (non-empty line), now parse tag and command (conn.go:222)
			var tag, name string
			if !dec.ExpectAtom(&tag) || !dec.ExpectSP() || !dec.ExpectAtom(&name) {
				err := dec.Err()
				if tt.errorContains != "" {
					if err == nil {
						t.Errorf("Expected error containing %q, got nil", tt.errorContains)
					} else if !strings.Contains(err.Error(), tt.errorContains) {
						t.Errorf("Expected error containing %q, got: %v", tt.errorContains, err)
					} else {
						t.Logf("Got correct error (not stale CRLF error): %v", err)
					}
				} else {
					t.Errorf("Unexpected error: %v", err)
				}
				return
			}

			// Successfully parsed
			if tt.errorContains != "" {
				t.Errorf("Expected error containing %q, but parsing succeeded with tag=%q name=%q", tt.errorContains, tag, name)
			} else {
				t.Logf("Successfully parsed: tag=%q name=%q", tag, name)
			}
		})
	}
}
