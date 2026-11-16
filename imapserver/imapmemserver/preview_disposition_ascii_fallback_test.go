package imapmemserver

import (
	"context"
	"strings"
	"testing"
)

// A MIME parser may return a normalized disposition together with a parameter
// error. The best-effort fallback must still check the original disposition
// token before treating a part as inline and exposing its text in PREVIEW.
func TestPreviewNonASCIIDispositionWithInvalidParameters(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition string
		want        string
	}{
		{name: "lookalike_with_missing_parameter_value", disposition: "İnlİne; bad"},
		{name: "lookalike_with_unterminated_filename", disposition: "İNLİNE; filename=\"unterminated"},
		{name: "ascii_with_missing_parameter_value", disposition: "inline; bad", want: "visible body"},
		{name: "ascii_with_unterminated_filename", disposition: "INLINE; filename=\"unterminated", want: "visible body"},
		{name: "unicode_filename", disposition: "INLINE; filename=\"café.txt\"", want: "visible body"},
		{name: "extended_unicode_filename", disposition: "inline; filename*=utf-8''caf%C3%A9.txt", want: "visible body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Type: text/plain; charset=utf-8\r\nContent-Disposition: " + tc.disposition + "\r\n\r\nvisible body\r\n"
			got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
			if err != nil || !available || got != tc.want {
				t.Errorf("Content-Disposition %q: preview = %q, available = %v, error = %v; want %q", tc.disposition, got, available, err, tc.want)
			}
		})
	}
}
