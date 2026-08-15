package imapclient

import (
	"testing"
	"time"
)

// TestOptionsTimeoutDefaults pins the rule that makes these fields safe to add:
// the zero value means "use the default", never "no timeout".
//
// setReadTimeout clears the deadline entirely for a non-positive duration, so
// passing the raw field through would leave every caller that predates these
// fields -- i.e. every caller writing Options{...} today -- waiting forever.
// That would reinstate the hang bounded by the response timeout, without a
// single compile error to warn anyone.
func TestOptionsTimeoutDefaults(t *testing.T) {
	tests := []struct {
		name                  string
		options               Options
		wantResponse, wantLit time.Duration
	}{
		{
			name:         "zero value uses the defaults",
			options:      Options{},
			wantResponse: respReadTimeout,
			wantLit:      literalReadTimeout,
		},
		{
			name:         "configured values are used",
			options:      Options{ResponseTimeout: 2 * time.Second, LiteralReadTimeout: 90 * time.Second},
			wantResponse: 2 * time.Second,
			wantLit:      90 * time.Second,
		},
		{
			name:         "negative falls back to the defaults",
			options:      Options{ResponseTimeout: -1, LiteralReadTimeout: -1},
			wantResponse: respReadTimeout,
			wantLit:      literalReadTimeout,
		},
		{
			name:         "each field is independent",
			options:      Options{ResponseTimeout: 5 * time.Second},
			wantResponse: 5 * time.Second,
			wantLit:      literalReadTimeout,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.options.responseTimeout(); got != tc.wantResponse {
				t.Errorf("responseTimeout() = %v, want %v", got, tc.wantResponse)
			}
			if got := tc.options.literalTimeout(); got != tc.wantLit {
				t.Errorf("literalTimeout() = %v, want %v", got, tc.wantLit)
			}
		})
	}
}
