package imapserver

import (
	"bufio"
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func TestReadSearchKeyUnknownKey(t *testing.T) {
	// An unquoted argument with a space leaves "test" as the next key.
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("test\r\n")), imapwire.ConnSideServer)

	var criteria imap.SearchCriteria
	err := readSearchKey(&Conn{}, &criteria, dec)
	if err == nil {
		t.Fatal("readSearchKey succeeded on an unknown search key")
	}

	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("readSearchKey returned %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Type != imap.StatusResponseTypeBad {
		t.Errorf("error type is %q, want %q", imapErr.Type, imap.StatusResponseTypeBad)
	}
	if imapErr.Code != imap.ResponseCodeClientBug {
		t.Errorf("error code is %q, want %q", imapErr.Code, imap.ResponseCodeClientBug)
	}
}
