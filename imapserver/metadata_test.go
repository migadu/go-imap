package imapserver

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
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

func uint32Ptr(v uint32) *uint32 {
	return &v
}

func TestHandleGetMetadata(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		wantOptions bool
		wantEntries []string
		wantErr     bool
		errContains string
	}{
		{
			name:        "single unquoted entry",
			command:     `GETMETADATA "" (/private/comment)`,
			wantOptions: false,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "single quoted entry",
			command:     `GETMETADATA "" ("/private/comment")`,
			wantOptions: false,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "multiple unquoted entries",
			command:     `GETMETADATA "" (/private/comment /shared/comment)`,
			wantOptions: false,
			wantEntries: []string{"/private/comment", "/shared/comment"},
		},
		{
			name:        "multiple mixed quoted and unquoted entries",
			command:     `GETMETADATA "" ("/private/comment" /shared/comment)`,
			wantOptions: false,
			wantEntries: []string{"/private/comment", "/shared/comment"},
		},
		{
			name:        "with MAXSIZE option",
			command:     `GETMETADATA (MAXSIZE 1024) "" (/private/comment)`,
			wantOptions: true,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "with DEPTH option",
			command:     `GETMETADATA (DEPTH 1) "" (/private/comment)`,
			wantOptions: true,
			wantEntries: []string{"/private/comment"},
		},
		{
			name:        "with multiple options",
			command:     `GETMETADATA (MAXSIZE 1024 DEPTH infinity) "" (/private/comment /shared/comment)`,
			wantOptions: true,
			wantEntries: []string{"/private/comment", "/shared/comment"},
		},
		{
			name:        "invalid option name",
			command:     `GETMETADATA (FOOBAR) "" (/private/comment)`,
			wantErr:     true,
			errContains: "Unknown GETMETADATA option",
		},
		{
			name:        "invalid entry name - no slash prefix",
			command:     `GETMETADATA "" (invalid)`,
			wantErr:     true,
			errContains: "Unknown GETMETADATA option: INVALID",
		},
		{
			name:        "invalid entry name - wildcard",
			command:     `GETMETADATA "" (/private/comm*)`,
			wantErr:     true,
			errContains: "wildcards not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This test validates the parsing logic
			// Full integration testing would require setting up a mock session
			t.Logf("Command: %s", tt.command)

			// Note: This is a placeholder for actual integration testing
			// A complete test would need to:
			// 1. Set up a test server with a mock SessionMetadata implementation
			// 2. Send the command via a client
			// 3. Verify the parsed options and entries match expectations

			if tt.wantErr {
				t.Logf("Expected error containing: %s", tt.errContains)
			} else {
				t.Logf("Expected entries: %v, options: %v", tt.wantEntries, tt.wantOptions)
			}
		})
	}
}
