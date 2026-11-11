package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// TestFetch_previewDecoding covers the shapes a PREVIEW response can take
// (RFC 8970 §6: "PREVIEW" SP nstring). NIL and the empty string mean
// different things, so they must decode differently, and a server that
// overruns the 256-character limit is cut down to it.
func TestFetch_previewDecoding(t *testing.T) {
	long := strings.Repeat("a", 300)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	client := imapclient.New(clientConn, nil)
	defer client.Close()

	commands := make(chan string, 1)
	go func() {
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 PREVIEW] ready\r\n")
		br := bufio.NewReader(serverConn)
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		commands <- line
		tag, _, _ := strings.Cut(line, " ")
		io.WriteString(serverConn, "* 1 FETCH (UID 1 PREVIEW NIL)\r\n")
		io.WriteString(serverConn, "* 2 FETCH (UID 2 PREVIEW \"\")\r\n")
		io.WriteString(serverConn, "* 3 FETCH (UID 3 PREVIEW \"Lunch?\")\r\n")
		io.WriteString(serverConn, "* 4 FETCH (UID 4 PREVIEW {7}\r\nCaf\xc3\xa9 ?)\r\n")
		io.WriteString(serverConn, "* 5 FETCH (UID 5 PREVIEW \""+long+"\")\r\n")
		io.WriteString(serverConn, "* 6 FETCH (UID 6 PREVIEW nil)\r\n")
		io.WriteString(serverConn, tag+" OK FETCH completed\r\n")
	}()
	// FETCH responses belong to a selected mailbox (RFC 9051 section 11.3).
	client.SetStateForTest(imap.ConnStateSelected)

	messages, err := client.Fetch(imap.SeqSetNum(1, 2, 3, 4, 5, 6), &imap.FetchOptions{
		UID:     true,
		Preview: &imap.FetchItemPreview{Lazy: true},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	}
	if command := <-commands; !strings.Contains(command, "(UID PREVIEW (LAZY))") {
		t.Errorf("command = %q, want it to ask for (UID PREVIEW (LAZY))", command)
	}
	if len(messages) != 6 {
		t.Fatalf("len(messages) = %v, want 6", len(messages))
	}

	str := func(s string) *string { return &s }
	for i, want := range []*string{nil, str(""), str("Lunch?"), str("Café ?"), str(long[:256]), nil} {
		got := messages[i].Preview
		switch {
		case want == nil && got != nil:
			t.Errorf("message %d: Preview = %q, want nil (NIL)", i+1, *got)
		case want != nil && got == nil:
			t.Errorf("message %d: Preview = nil, want %q", i+1, *want)
		case want != nil && *got != *want:
			t.Errorf("message %d: Preview = %q (%d characters), want %q", i+1, *got, utf8.RuneCountInString(*got), *want)
		}
	}
}

func TestFetch_preview(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapPreview) {
		t.Skip("server doesn't support PREVIEW")
	}

	messages, err := client.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		Preview: &imap.FetchItemPreview{},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	} else if len(messages) != 1 {
		t.Fatalf("len(messages) = %v, want 1", len(messages))
	}
	if got := messages[0].Preview; got == nil || *got != "This is my letter!" {
		t.Errorf("Preview = %v, want %q", got, "This is my letter!")
	}
}
