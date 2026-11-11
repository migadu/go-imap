package imapserver_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// RFC 9051 lets a client request a partial at a number64 offset. The origin
// octet the server echoes must be that offset, not its low 32 bits: a client
// matches the response to its request by that number, and an origin of 5
// for a request at 2^32+5 claims the bytes start somewhere they do not.
func TestFetchPartialOffsetEchoedInFull(t *testing.T) {
	w := dialParseServer(t, nil)

	msg := "Subject: hi\r\n\r\nhello world\r\n"
	fmt.Fprintf(w.conn, "a2 APPEND INBOX {%d}\r\n", len(msg))
	if l := w.line(); !strings.HasPrefix(l, "+") {
		t.Fatalf("APPEND continuation: got %q", l)
	}
	fmt.Fprint(w.conn, msg+"\r\n")
	if got := w.until("a2"); !strings.HasPrefix(got, "a2 OK") {
		t.Fatalf("APPEND: %q", got)
	}
	fmt.Fprint(w.conn, "a3 SELECT INBOX\r\n")
	if got := w.until("a3"); !strings.HasPrefix(got, "a3 OK") {
		t.Fatalf("SELECT: %q", got)
	}

	for _, tc := range []struct {
		tag, item, want string
	}{
		{"a4", "BODY[]<4294967301.10>", " BODY[]<4294967301> {0}"},
		{"a5", "BODY[]<4294967296.10>", " BODY[]<4294967296> {0}"},
		{"a6", "BINARY[]<4294967301.10>", " BINARY[]<4294967301> ~{0}"},
	} {
		t.Run(tc.item, func(t *testing.T) {
			fmt.Fprintf(w.conn, "%s FETCH 1 %s\r\n", tc.tag, tc.item)
			var data string
			for {
				l := w.line()
				if strings.HasPrefix(l, tc.tag+" ") {
					if !strings.HasPrefix(l, tc.tag+" OK") {
						t.Fatalf("FETCH answered %q; want OK", l)
					}
					break
				}
				if strings.HasPrefix(l, "* 1 FETCH (") && strings.Contains(l, tc.item[:strings.IndexByte(tc.item, '<')]+"<") {
					data = l
				}
			}
			if !strings.HasSuffix(data, tc.want) {
				t.Errorf("data item line %q; want it to end with %q", data, tc.want)
			}
		})
	}
}

// The same offset through the client library: the client asks for a
// number64 origin and must find the section the server sends back for it.
func TestFetchPartialOffsetRoundTrip(t *testing.T) {
	mem := imapmemserver.New()
	u := imapmemserver.NewUser("u", "p")
	if err := u.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln) //nolint:errcheck

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := imapclient.New(conn, nil)
	t.Cleanup(func() { c.Close() })
	if err := c.Login("u", "p").Wait(); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	msg := "Subject: hi\r\n\r\nhello world\r\n"
	ac := c.Append("INBOX", int64(len(msg)), nil)
	if _, err := ac.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("APPEND: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}

	const offset int64 = 1<<32 + 5
	section := &imap.FetchItemBodySection{Partial: &imap.SectionPartial{Offset: offset, Size: 10}}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		t.Fatalf("FETCH: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	var found bool
	for _, s := range msgs[0].BodySection {
		if s.Section.Partial == nil {
			t.Errorf("section came back without a partial")
			continue
		}
		if s.Section.Partial.Offset == offset {
			found = true
		} else {
			t.Errorf("section came back at offset %d, want %d", s.Section.Partial.Offset, offset)
		}
	}
	if !found {
		t.Errorf("no body section at offset %d in %d section(s)", offset, len(msgs[0].BodySection))
	}
}
