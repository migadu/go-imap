package imapclient_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// newCatenateServer starts a scripted server offering caps. It reads one
// command, literals included, asks for the synchronizing ones, answers with
// tagged, and sends what it read on the returned channel.
func newCatenateServer(t *testing.T, caps, tagged string) (*imapclient.Client, <-chan string) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })

	gotCh := make(chan string, 1)
	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY "+caps+"] ready\r\n")
		var got strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			got.WriteString(line)
			announce := strings.TrimSuffix(line, "}\r\n")
			start := strings.LastIndexByte(announce, '{')
			if announce == line || start < 0 {
				break
			}
			size := announce[start+1:]
			if nonSync := strings.TrimSuffix(size, "+"); nonSync != size {
				size = nonSync
			} else {
				io.WriteString(serverConn, "+ go ahead\r\n")
			}
			n, err := strconv.Atoi(size)
			if err != nil {
				return
			}
			lit := make([]byte, n)
			if _, err := io.ReadFull(br, lit); err != nil {
				return
			}
			got.Write(lit)
		}
		gotCh <- got.String()
		io.WriteString(serverConn, "T1 "+tagged+"\r\n")
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	return client, gotCh
}

// RFC 4469 §5:
//
//	append-data =/ "CATENATE" SP "(" cat-part *(SP cat-part) ")"
//	cat-part = text-literal / url
//	text-literal = "TEXT" SP literal
//	url = "URL" SP astring
//
// Upstream request: emersion/go-imap#315.
func TestAppendCatenate(t *testing.T) {
	for _, tc := range []struct {
		name, caps, want string
	}{
		{
			name: "synchronizing literal",
			caps: "IMAP4rev1 CATENATE",
			want: `T1 APPEND "Drafts" (\Draft) CATENATE (URL "/Drafts;UIDVALIDITY=385759045/;UID=20/;section=HEADER" TEXT {5}` + "\r\n" +
				`hello URL "/Drafts;UIDVALIDITY=385759045/;UID=20/;section=2")` + "\r\n",
		},
		{
			name: "non-synchronizing literal",
			caps: "IMAP4rev1 CATENATE LITERAL-",
			want: `T1 APPEND "Drafts" (\Draft) CATENATE (URL "/Drafts;UIDVALIDITY=385759045/;UID=20/;section=HEADER" TEXT {5+}` + "\r\n" +
				`hello URL "/Drafts;UIDVALIDITY=385759045/;UID=20/;section=2")` + "\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, gotCh := newCatenateServer(t, tc.caps, "OK [APPENDUID 385759045 45] append completed")

			cmd := client.AppendCatenate("Drafts", &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft}})
			cmd.URL("/Drafts;UIDVALIDITY=385759045/;UID=20/;section=HEADER")
			text := cmd.Text(5)
			if _, err := io.WriteString(text, "hello"); err != nil {
				t.Fatalf("Text().Write() = %v", err)
			}
			if err := text.Close(); err != nil {
				t.Fatalf("Text().Close() = %v", err)
			}
			cmd.URL("/Drafts;UIDVALIDITY=385759045/;UID=20/;section=2")
			if err := cmd.Close(); err != nil {
				t.Fatalf("Close() = %v", err)
			}
			data, err := cmd.Wait()
			if err != nil {
				t.Fatalf("Wait() = %v", err)
			}
			if got := <-gotCh; got != tc.want {
				t.Errorf("client sent\n%q, want\n%q", got, tc.want)
			}
			if data.UIDValidity != 385759045 || data.UID != 45 {
				t.Errorf("AppendData = %+v, want UIDVALIDITY 385759045 and UID 45", data)
			}
		})
	}
}

// RFC 4469 §4.1: "The BADURL response code is returned if the APPEND fails to
// process one of the specified URLs."
func TestAppendCatenateBadURL(t *testing.T) {
	client, _ := newCatenateServer(t, "IMAP4rev1 CATENATE", "NO [BADURL /INBOX;UIDVALIDITY=785799047/;UID=113330/;section=1.5.9] CATENATE append has failed, one message expunged")

	cmd := client.AppendCatenate("Sent", nil)
	cmd.URL("/INBOX;UIDVALIDITY=785799047/;UID=113330/;section=1.5.9")
	if err := cmd.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	_, err := cmd.Wait()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("Wait() = %v, want an *imap.Error", err)
	}
	if imapErr.Type != imap.StatusResponseTypeNo || imapErr.Code != imap.ResponseCodeBadURL {
		t.Errorf("Wait() = %v, want NO with the BADURL response code", err)
	}
}
