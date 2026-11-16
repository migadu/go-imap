package imapserver

import (
	"bufio"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
	"strings"
	"testing"
)

func TestSearchPreservesHeaderOrigin(t *testing.T) {
	for _, tc := range []struct {
		wire     string
		explicit bool
	}{
		{`HEADER Cc ""`, true}, {`CC ""`, false}, {`HEADER From "x"`, true}, {`FROM "x"`, false}, {`HEADER Subject ""`, true}, {`SUBJECT ""`, false},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			var c imap.SearchCriteria
			d := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(tc.wire+"\r\n")), imapwire.ConnSideServer)
			if err := readSearchKey(&Conn{}, &c, d); err != nil {
				t.Fatal(err)
			}
			if len(c.Header) != 1 || c.Header[0].IsHeader != tc.explicit {
				t.Fatalf("criteria=%+v", c.Header)
			}
		})
	}
}
