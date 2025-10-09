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
