package imapclient

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func encodeBodySection(item *imap.FetchItemBodySection) string {
	buf := &bytes.Buffer{}
	bw := bufio.NewWriter(buf)
	enc := imapwire.NewEncoder(bw, imapwire.ConnSideClient)
	writeFetchItemBodySection(enc, item)
	bw.Flush()
	return buf.String()
}

// TestWriteFetchItemBodySectionHeaderFields pins how header field names go out
// on the wire.
//
// header-fld-name is an astring, so both "Message-ID" and Message-ID are legal,
// but the atom form is what every widely deployed client sends and therefore
// the better-tested path through a server's parser. mailo.com answers
// BODY[HEADER.FIELDS ("Message-ID")] with an empty body while answering the
// unquoted form correctly.
//
// Upstream: emersion/go-imap#589 by rakoo.
func TestWriteFetchItemBodySectionHeaderFields(t *testing.T) {
	tests := []struct {
		name string
		item *imap.FetchItemBodySection
		want string
	}{
		{
			name: "header fields as atoms",
			item: &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID", "Subject"}},
			want: "BODY[HEADER.FIELDS (Message-ID Subject)]",
		},
		{
			name: "header fields not",
			item: &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFieldsNot: []string{"Received"}},
			want: "BODY[HEADER.FIELDS.NOT (Received)]",
		},
		{
			name: "peek",
			item: &imap.FetchItemBodySection{Peek: true, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"From"}},
			want: "BODY.PEEK[HEADER.FIELDS (From)]",
		},
		{
			// Not a valid atom, so it must keep the quoted form rather than
			// being written bare and corrupting the command.
			name: "name needing quotes falls back to a string",
			item: &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"X Weird"}},
			want: `BODY[HEADER.FIELDS ("X Weird")]`,
		},
		{
			name: "name with a parenthesis falls back to a string",
			item: &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"X-(a)"}},
			want: `BODY[HEADER.FIELDS ("X-(a)")]`,
		},
		{
			name: "whole message unaffected",
			item: &imap.FetchItemBodySection{},
			want: "BODY[]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeBodySection(tc.item); got != tc.want {
				t.Errorf("writeFetchItemBodySection() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEncoderAString(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Message-ID", "Message-ID"},
		{"Subject", "Subject"},
		{"X-Custom_Header.1", "X-Custom_Header.1"},
		{"", `""`},
		{"has space", `"has space"`},
		{`has"quote`, `"has\"quote"`},
		{"has(paren", `"has(paren"`},
		{"has]bracket", `"has]bracket"`},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			buf := &bytes.Buffer{}
			bw := bufio.NewWriter(buf)
			enc := imapwire.NewEncoder(bw, imapwire.ConnSideClient)
			enc.AString(tc.in)
			bw.Flush()
			if got := buf.String(); got != tc.want {
				t.Errorf("AString(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
