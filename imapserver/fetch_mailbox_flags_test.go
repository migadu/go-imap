package imapserver

import (
	"bufio"
	"bytes"
	"net"
	"testing"

	"github.com/migadu/go-imap"
)

// A command's FLAGS update goes out where the backend writes it: ahead of the
// FETCH responses that use the keyword it announces.
func TestFetchWriterWriteMailboxFlags(t *testing.T) {
	local, peer := net.Pipe()
	t.Cleanup(func() { local.Close(); peer.Close() })
	var out bytes.Buffer
	conn := &Conn{conn: local, bw: bufio.NewWriter(&out), server: New(&Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}})}
	fw := &FetchWriter{conn: conn}

	if err := fw.WriteMailboxFlags([]imap.Flag{imap.FlagSeen, "$New"}); err != nil {
		t.Fatal(err)
	}
	w := fw.CreateMessage(1)
	w.WriteFlags([]imap.Flag{"$New"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	const want = "* FLAGS (\\Seen $New)\r\n* 1 FETCH (FLAGS ($New))\r\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
