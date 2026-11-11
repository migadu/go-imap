package imapclient

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// RFC 5032 §3: search-key =/ ( "OLDER" / "YOUNGER" ) SP nz-number
func TestWriteSearchKeyWithin(t *testing.T) {
	for _, tc := range []struct {
		criteria imap.SearchCriteria
		want     string
	}{
		{imap.SearchCriteria{Younger: 259200}, "YOUNGER 259200"},
		{imap.SearchCriteria{Older: 60}, "OLDER 60"},
		{imap.SearchCriteria{Younger: 259200, Older: 60}, "YOUNGER 259200 OLDER 60"},
		{imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}, Younger: 259200}, "UNSEEN YOUNGER 259200"},
		{imap.SearchCriteria{Not: []imap.SearchCriteria{{Older: 60}}}, "NOT (OLDER 60)"},
	} {
		buf := &bytes.Buffer{}
		bw := bufio.NewWriter(buf)
		writeSearchKey(imapwire.NewEncoder(bw, imapwire.ConnSideClient), &tc.criteria, false)
		bw.Flush()
		if got := buf.String(); got != tc.want {
			t.Errorf("writeSearchKey(%+v) = %q, want %q", tc.criteria, got, tc.want)
		}
	}
}
