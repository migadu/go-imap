package imapclient

import (
	"bufio"
	"bytes"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
	"testing"
)

func TestSearchEncoderPreservesExplicitHeader(t *testing.T) {
	for _, tc := range []struct {
		explicit bool
		want     string
	}{{false, `CC ""`}, {true, `HEADER "Cc" ""`}} {
		var b bytes.Buffer
		w := bufio.NewWriter(&b)
		enc := imapwire.NewEncoder(w, imapwire.ConnSideClient)
		writeSearchKey(enc, &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Cc", IsHeader: tc.explicit}}}, false)
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if b.String() != tc.want {
			t.Fatalf("wire=%q want%q", b.String(), tc.want)
		}
	}
}
