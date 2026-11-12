package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/migadu/go-imap/internal/imapwire"
)

func TestReadIDNILCase(t *testing.T) {
	for _, token := range []string{"NIL", "nil", "nIl"} {
		t.Run(token, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(" "+token+"\r\n")), imapwire.ConnSideServer)
			data, err := readID(dec)
			if err != nil || data != nil || !dec.ExpectCRLF() {
				t.Fatalf("ID %s=(%v,%v), decoder=%v", token, data, err, dec.Err())
			}
		})
		t.Run("value/"+token, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(` ("name" `+token+` "vendor" "NiL")`+"\r\n")), imapwire.ConnSideServer)
			data, err := readID(dec)
			if err != nil || data == nil || data.Name != "" || data.Vendor != "NiL" || !dec.ExpectCRLF() {
				t.Fatalf("ID value %s=(%v,%v), decoder=%v", token, data, err, dec.Err())
			}
		})
	}
}
