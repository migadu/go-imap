package imapmemserver

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGeneratePreviewMIMESelection(t *testing.T) {
	plain := previewTestEntity("text/plain; charset=utf-8", "", "", "Actual inline body")
	attachment := "Content-Disposition: attachment; filename=attached.mime\r\n" + previewTestMultipart("attached", previewTestEntity("text/plain", "", "", "Attachment contents"))
	unknownMultipart := "Content-Disposition: X-PRIVATE; filename=private.mime\r\n" + previewTestMultipart("private", previewTestEntity("text/plain", "", "inline", "Private attachment"))
	for _, tc := range []struct{ name, raw, want string }{
		{name: "multipart attachment", raw: previewTestMultipart("mixed", attachment, plain), want: "Actual inline body"},
		{name: "root multipart attachment", raw: attachment, want: ""},
		{name: "nested multipart attachment", raw: previewTestMultipart("outer", previewTestMultipart("inner", attachment, plain)), want: "Actual inline body"},
		{name: "only multipart attachment", raw: previewTestMultipart("only", attachment), want: ""},
		{name: "unknown disposition", raw: previewTestMultipart("mixed", previewTestEntity("text/plain", "", "x-private; filename=private.txt", "Private attachment"), plain), want: "Actual inline body"},
		{name: "unknown disposition malformed parameters", raw: previewTestMultipart("mixed", previewTestEntity("text/plain", "", "X-PRIVATE; filename=private note.txt", "Private attachment"), plain), want: "Actual inline body"},
		{name: "malformed disposition without type", raw: previewTestMultipart("mixed", previewTestEntity("text/plain", "", "; filename=private.txt", "Private attachment"), plain), want: "Actual inline body"},
		{name: "attachment malformed parameters", raw: previewTestMultipart("mixed", previewTestEntity("text/plain", "", "ATTACHMENT; filename=private note.txt", "Private attachment"), plain), want: "Actual inline body"},
		{name: "unknown multipart disposition", raw: previewTestMultipart("mixed", unknownMultipart, plain), want: "Actual inline body"},
		{name: "unknown root multipart disposition", raw: unknownMultipart, want: ""},
		{name: "unknown root text disposition", raw: previewTestEntity("text/plain", "", "x-private", "Private attachment"), want: ""},
		{name: "absent disposition", raw: plain, want: "Actual inline body"},
		{name: "inline disposition", raw: previewTestEntity("text/plain", "", "INLINE; filename=note.txt", "Inline body"), want: "Inline body"},
		{name: "inline malformed parameters", raw: previewTestEntity("text/plain", "", "Inline; filename=inline note.txt", "Inline body"), want: "Inline body"},
		{name: "inline multipart disposition", raw: "Content-Disposition: INLINE\r\n" + previewTestMultipart("inline", plain), want: "Actual inline body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, available, err := generatePreview(context.Background(), strings.NewReader(tc.raw))
			if err != nil || !available || text != tc.want {
				t.Fatalf("preview=(%q,%v,%v), want(%q,true,nil)", text, available, err, tc.want)
			}
		})
	}
}

func previewSelectionDigest(boundary string, parts ...string) string {
	return strings.Replace(previewTestMultipart(boundary, parts...), "multipart/mixed", "multipart/digest", 1)
}

func TestGeneratePreviewDigestDefaults(t *testing.T) {
	plain := previewTestEntity("text/plain; charset=utf-8", "", "", "Actual inline body")
	messageBody := "From: sender@example.com\r\nSubject: Encapsulated message\r\nContent-Type: text/plain\r\n\r\nDigest message text"
	untypedMessage := previewTestEntity("", "", "", messageBody)
	digest := previewSelectionDigest("digest", untypedMessage)
	for _, tc := range []struct{ name, raw, want string }{
		{name: "implicit message falls through to body", raw: previewTestMultipart("mixed", digest, plain), want: "Actual inline body"},
		{name: "only implicit messages", raw: digest, want: ""},
		{name: "explicit message also unsupported", raw: previewSelectionDigest("digest", previewTestEntity("message/rfc822", "", "", messageBody)), want: ""},
		{name: "explicit plain remains eligible", raw: previewSelectionDigest("digest", untypedMessage, plain), want: "Actual inline body"},
		{name: "explicit plain still decoded", raw: previewSelectionDigest("digest", previewTestEntity("text/plain; charset=utf-8", "quoted-printable", "", "caf=C3=A9")), want: "café"},
		{name: "default applies only to direct children", raw: previewSelectionDigest("digest", previewTestMultipart("inner", previewTestEntity("", "", "", "Mixed default plain"))), want: "Mixed default plain"},
		{name: "unknown transfer coding on implicit message", raw: previewSelectionDigest("digest", previewTestEntity("", "x-private", "", messageBody), plain), want: "Actual inline body"},
		{name: "root default remains plain", raw: previewTestEntity("", "", "", "Root default plain"), want: "Root default plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, available, err := generatePreview(context.Background(), strings.NewReader(tc.raw))
			if err != nil || !available || text != tc.want {
				t.Fatalf("preview=(%q,%v,%v), want(%q,true,nil)", text, available, err, tc.want)
			}
		})
	}
}

// Attachment subtrees are not preview candidates, even when their descendants
// have explicitly inline dispositions or exceed the MIME traversal limits.
func TestGeneratePreviewSkipsAttachmentSubtrees(t *testing.T) {
	deep := "Content-Disposition: attachment\r\n" + previewTestNested(maxPreviewMIMEDepth*4)
	plain := previewTestEntity("text/plain", "", "", "Actual inline body")
	raw := previewTestMultipart("mixed", deep, plain)
	if len(raw) >= maxPreviewRawBytes {
		t.Fatal("depth fixture also exceeds raw budget")
	}
	text, available, err := generatePreview(context.Background(), strings.NewReader(raw))
	if err != nil || !available || text != "Actual inline body" {
		t.Fatalf("preview=(%q,%v,%v), want inline body after deep attachment", text, available, err)
	}

	// A root attachment needs no body reads or draining to report no candidate.
	counter := &previewCountingReader{r: strings.NewReader(deep)}
	text, available, err = generatePreview(context.Background(), counter)
	if err != nil || !available || text != "" || counter.bytes >= len(deep) {
		t.Fatalf("root attachment preview=(%q,%v,%v), read %d of %d bytes", text, available, err, counter.bytes, len(deep))
	}
}

// Advancing past an excluded multipart or digest still consumes raw input, so
// their bodies remain covered by the aggregate limit and cancellation checks.
func TestGeneratePreviewExcludedMIMEInputLimits(t *testing.T) {
	plain := previewTestEntity("text/plain", "", "", "Actual inline body")
	for _, tc := range []struct{ name, first string }{
		{name: "multipart attachment", first: "Content-Disposition: attachment\r\n" + previewTestMultipart("attached", previewTestEntity("text/plain", "", "", strings.Repeat("x", maxPreviewRawBytes+1)))},
		{name: "digest implicit message", first: previewSelectionDigest("digest", previewTestEntity("", "", "", "From: sender@example.com\r\n\r\n"+strings.Repeat("x", maxPreviewRawBytes+1)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := previewTestMultipart("mixed", tc.first, plain)
			counter := &previewCountingReader{r: strings.NewReader(raw)}
			text, available, err := generatePreview(context.Background(), counter)
			if err != nil || available || text != "" || counter.bytes != maxPreviewRawBytes {
				t.Fatalf("limited preview=(%q,%v,%v), read %d raw bytes", text, available, err, counter.bytes)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			counter = &previewCountingReader{r: strings.NewReader(raw)}
			counter.afterRead = func() {
				if counter.calls == 2 {
					cancel()
				}
			}
			text, available, err = generatePreview(ctx, counter)
			if text != "" || available || !errors.Is(err, context.Canceled) || counter.calls != 2 {
				t.Fatalf("cancelled preview=(%q,%v,%v), source reads=%d", text, available, err, counter.calls)
			}
		})
	}
}
