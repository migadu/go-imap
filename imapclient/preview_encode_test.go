package imapclient

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func encodeFetchItems(numKind imapwire.NumKind, options *imap.FetchOptions) string {
	buf := &bytes.Buffer{}
	bw := bufio.NewWriter(buf)
	enc := imapwire.NewEncoder(bw, imapwire.ConnSideClient)
	writeFetchItems(enc, numKind, options)
	bw.Flush()
	return buf.String()
}

// TestWriteFetchItemsPreview pins the PREVIEW data item on the wire (RFC 8970
// §6): bare, or followed by its LAZY modifier list.
func TestWriteFetchItemsPreview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		numKind imapwire.NumKind
		preview *imap.FetchItemPreview
		want    string
	}{
		{name: "preview", numKind: imapwire.NumKindSeq, preview: &imap.FetchItemPreview{}, want: "(PREVIEW)"},
		{name: "lazy", numKind: imapwire.NumKindSeq, preview: &imap.FetchItemPreview{Lazy: true}, want: "(PREVIEW (LAZY))"},
		{name: "after UID", numKind: imapwire.NumKindUID, preview: &imap.FetchItemPreview{Lazy: true}, want: "(UID PREVIEW (LAZY))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := &imap.FetchOptions{Preview: tc.preview}
			if got := encodeFetchItems(tc.numKind, options); got != tc.want {
				t.Errorf("writeFetchItems() = %q, want %q", got, tc.want)
			}
			if !hasFetchItems(options) {
				t.Errorf("hasFetchItems() = false for a PREVIEW-only request")
			}
		})
	}
}
