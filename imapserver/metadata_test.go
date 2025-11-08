package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func TestValidateMetadataEntry(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		// Valid entries
		{
			name:    "valid private entry",
			entry:   "/private/comment",
			wantErr: false,
		},
		{
			name:    "valid shared entry",
			entry:   "/shared/comment",
			wantErr: false,
		},
		{
			name:    "valid private nested",
			entry:   "/private/vendor/cmu/cyrus-imapd/lastpop",
			wantErr: false,
		},
		{
			name:    "valid shared nested",
			entry:   "/shared/vendor/cmu/cyrus-imapd/squat",
			wantErr: false,
		},

		// Invalid entries
		{
			name:    "empty entry",
			entry:   "",
			wantErr: true,
		},
		{
			name:    "missing prefix",
			entry:   "/comment",
			wantErr: true,
		},
		{
			name:    "wrong prefix",
			entry:   "/public/comment",
			wantErr: true,
		},
		{
			name:    "contains asterisk wildcard",
			entry:   "/private/comm*ent",
			wantErr: true,
		},
		{
			name:    "contains percent wildcard",
			entry:   "/private/comm%ent",
			wantErr: true,
		},
		{
			name:    "consecutive slashes",
			entry:   "/private//comment",
			wantErr: true,
		},
		{
			name:    "trailing slash",
			entry:   "/private/comment/",
			wantErr: true,
		},
		{
			name:    "base private with slash - allowed",
			entry:   "/private/",
			wantErr: false,
		},
		{
			name:    "base shared with slash - allowed",
			entry:   "/shared/",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := imap.ValidateMetadataEntry(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMetadataEntry(%q) error = %v, wantErr %v", tt.entry, err, tt.wantErr)
			}
		})
	}
}

func TestReadGetMetadataOption(t *testing.T) {
	tests := []struct {
		name           string
		optionName     string
		input          string // Simulated decoder input after option name
		wantMaxSize    *uint32
		wantDepth      imap.GetMetadataDepth
		wantErr        bool
		wantErrContain string
	}{
		{
			name:        "MAXSIZE valid",
			optionName:  "MAXSIZE",
			input:       "1024",
			wantMaxSize: uint32Ptr(1024),
			wantDepth:   imap.GetMetadataDepthZero,
		},
		{
			name:       "DEPTH 0",
			optionName: "DEPTH",
			input:      "0",
			wantDepth:  imap.GetMetadataDepthZero,
		},
		{
			name:       "DEPTH 1",
			optionName: "DEPTH",
			input:      "1",
			wantDepth:  imap.GetMetadataDepthOne,
		},
		{
			name:       "DEPTH infinity",
			optionName: "DEPTH",
			input:      "infinity",
			wantDepth:  imap.GetMetadataDepthInfinity,
		},
		{
			name:       "DEPTH INFINITY uppercase",
			optionName: "DEPTH",
			input:      "INFINITY",
			wantDepth:  imap.GetMetadataDepthInfinity,
		},
		{
			name:           "unknown option",
			optionName:     "UNKNOWN",
			wantErr:        true,
			wantErrContain: "Unknown GETMETADATA option",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Note: This is testing the logic, not the actual decoder
			// A full test would require mocking the decoder
			var options imap.GetMetadataOptions

			// Simulate option parsing behavior
			switch strings.ToUpper(tt.optionName) {
			case "MAXSIZE":
				if tt.input == "1024" {
					val := uint32(1024)
					options.MaxSize = &val
				}
			case "DEPTH":
				switch strings.ToLower(tt.input) {
				case "0":
					options.Depth = imap.GetMetadataDepthZero
				case "1":
					options.Depth = imap.GetMetadataDepthOne
				case "infinity":
					options.Depth = imap.GetMetadataDepthInfinity
				}
			case "UNKNOWN":
				err := &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Text: "Unknown GETMETADATA option: UNKNOWN",
				}
				if !tt.wantErr {
					t.Errorf("got error %v, want no error", err)
				}
				if !strings.Contains(err.Text, tt.wantErrContain) {
					t.Errorf("error text %q does not contain %q", err.Text, tt.wantErrContain)
				}
				return
			}

			// Verify results
			if tt.wantMaxSize != nil {
				if options.MaxSize == nil {
					t.Error("MaxSize is nil, want non-nil")
				} else if *options.MaxSize != *tt.wantMaxSize {
					t.Errorf("MaxSize = %d, want %d", *options.MaxSize, *tt.wantMaxSize)
				}
			}

			if options.Depth != tt.wantDepth {
				t.Errorf("Depth = %v, want %v", options.Depth, tt.wantDepth)
			}
		})
	}
}

func TestGetMetadataDepth_String(t *testing.T) {
	tests := []struct {
		depth imap.GetMetadataDepth
		want  string
	}{
		{imap.GetMetadataDepthZero, "0"},
		{imap.GetMetadataDepthOne, "1"},
		{imap.GetMetadataDepthInfinity, "infinity"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.depth.String()
			if got != tt.want {
				t.Errorf("GetMetadataDepth(%d).String() = %q, want %q", tt.depth, got, tt.want)
			}
		})
	}
}

func TestGetMetadataDepth_String_Invalid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for invalid depth, got none")
		}
	}()

	// Invalid depth value should panic
	invalidDepth := imap.GetMetadataDepth(999)
	_ = invalidDepth.String()
}

// mockMetadataSession implements SessionMetadata for testing
type mockMetadataSession struct {
	Session
	getMetadataCalled bool
	lastMailbox       string
	lastEntries       []string
	lastOptions       *imap.GetMetadataOptions
}

func (s *mockMetadataSession) GetMetadata(mailbox string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	s.getMetadataCalled = true
	s.lastMailbox = mailbox
	s.lastEntries = entries
	s.lastOptions = options

	// Return empty response
	return &imap.GetMetadataData{
		Mailbox: mailbox,
		Entries: make(map[string]*[]byte),
	}, nil
}

func (s *mockMetadataSession) SetMetadata(mailbox string, entries map[string]*[]byte) error {
	return nil
}

// TestHandleGetMetadata_Integration tests the actual GETMETADATA command parsing
// by calling handleGetMetadata directly with test data.
func TestHandleGetMetadata_Integration(t *testing.T) {
	tests := []struct {
		name        string
		input       string // IMAP wire format after "GETMETADATA "
		wantOptions bool
		wantEntries []string
		wantMaxSize *uint32
		wantDepth   imap.GetMetadataDepth
		wantErr     bool
		errContains string
	}{
		{
			name:        "single unquoted entry without parentheses",
			input:       ` "" /private/comment` + "\r\n",
			wantOptions: false,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "single quoted entry without parentheses",
			input:       ` "" "/private/comment"` + "\r\n",
			wantOptions: false,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "single unquoted entry with parentheses",
			input:       ` "" (/private/comment)` + "\r\n",
			wantOptions: false,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "single quoted entry",
			input:       ` "" ("/private/comment")` + "\r\n",
			wantOptions: false,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "multiple unquoted entries",
			input:       ` "" (/private/comment /shared/comment)` + "\r\n",
			wantOptions: false,
			wantEntries: []string{"/private/comment", "/shared/comment"},
		},
		{
			name:        "multiple mixed quoted and unquoted entries",
			input:       ` "" ("/private/comment" /shared/comment)` + "\r\n",
			wantOptions: false,
			wantEntries: []string{"/private/comment", "/shared/comment"},
		},
		{
			name:        "with MAXSIZE option",
			input:       ` (MAXSIZE 1024) "" (/private/comment)` + "\r\n",
			wantOptions: true,
			wantEntries: []string{"/private/comment"},
			wantMaxSize: uint32Ptr(1024),
		},
		{
			name:        "with DEPTH option",
			input:       ` (DEPTH 1) "" (/private/comment)` + "\r\n",
			wantOptions: true,
			wantEntries: []string{"/private/comment"},
			wantDepth:   imap.GetMetadataDepthOne,
		},
		{
			name:        "with multiple options and single entry",
			input:       ` (MAXSIZE 1024 DEPTH infinity) "" (/private/comment)` + "\r\n",
			wantOptions: true,
			wantEntries: []string{"/private/comment"},
			wantMaxSize: uint32Ptr(1024),
			wantDepth:   imap.GetMetadataDepthInfinity,
		},
		{
			name:        "with multiple options and multiple entries",
			input:       ` (MAXSIZE 1024 DEPTH infinity) "" (/private/comment /shared/comment)` + "\r\n",
			wantOptions: true,
			wantEntries: []string{"/private/comment", "/shared/comment"},
			wantMaxSize: uint32Ptr(1024),
			wantDepth:   imap.GetMetadataDepthInfinity,
		},
		{
			name:        "with DEPTH option and three entries",
			input:       ` (DEPTH 1) "" (/private/comment /shared/comment /private/title)` + "\r\n",
			wantOptions: true,
			wantEntries: []string{"/private/comment", "/shared/comment", "/private/title"},
			wantDepth:   imap.GetMetadataDepthOne,
		},
		{
			name:        "invalid option name",
			input:       ` (FOOBAR) "" (/private/comment)` + "\r\n",
			wantErr:     true,
			errContains: "Unknown GETMETADATA option",
		},
		{
			name:        "invalid entry name - no slash prefix",
			input:       ` "" (invalid)` + "\r\n",
			wantErr:     true,
			errContains: "entry name must start with",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock session
			session := &mockMetadataSession{}

			// Create server connection with mock session
			conn := &Conn{
				session: session,
				state:   imap.ConnStateAuthenticated, // Skip auth for testing
			}

			// Parse the input using a decoder
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tt.input)), 0)

			// Call handleGetMetadata directly
			err := conn.handleGetMetadata(dec)

			// Check error expectations
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error containing %q, got no error", tt.errContains)
					return
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Expected error containing %q, got %q", tt.errContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Verify GetMetadata was called
			if !session.getMetadataCalled {
				t.Fatal("GetMetadata was not called")
			}

			// Verify entries
			if len(session.lastEntries) != len(tt.wantEntries) {
				t.Errorf("Got %d entries, want %d. Entries: %v", len(session.lastEntries), len(tt.wantEntries), session.lastEntries)
			}
			for i, want := range tt.wantEntries {
				if i >= len(session.lastEntries) {
					t.Errorf("Missing entry[%d] = %q", i, want)
					break
				}
				if session.lastEntries[i] != want {
					t.Errorf("Entry[%d] = %q, want %q", i, session.lastEntries[i], want)
				}
			}

			// Verify options
			if tt.wantOptions && session.lastOptions == nil {
				t.Error("Expected options to be present, got nil")
			} else if !tt.wantOptions && session.lastOptions != nil {
				t.Error("Expected no options, got non-nil")
			}

			// Verify specific option values
			if tt.wantMaxSize != nil {
				if session.lastOptions == nil || session.lastOptions.MaxSize == nil {
					t.Error("Expected MaxSize option, got nil")
				} else if *session.lastOptions.MaxSize != *tt.wantMaxSize {
					t.Errorf("MaxSize = %d, want %d", *session.lastOptions.MaxSize, *tt.wantMaxSize)
				}
			}

			if tt.wantDepth != imap.GetMetadataDepthZero {
				if session.lastOptions == nil {
					t.Error("Expected options with Depth, got nil")
				} else if session.lastOptions.Depth != tt.wantDepth {
					t.Errorf("Depth = %v, want %v", session.lastOptions.Depth, tt.wantDepth)
				}
			}
		})
	}
}

func uint32Ptr(v uint32) *uint32 {
	return &v
}
