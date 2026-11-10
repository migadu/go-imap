package imapserver_test

import (
	"bytes"
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// rfc5256Example is the first example of RFC 5256 section 4, followed by its
// second, a thread whose parent does not match the search.
var rfc5256Example = []imap.ThreadData{
	{Chain: []uint32{2}},
	{Chain: []uint32{3, 6}, SubThreads: []imap.ThreadData{
		{Chain: []uint32{4, 23}},
		{Chain: []uint32{44, 7, 96}},
	}},
	{SubThreads: []imap.ThreadData{
		{Chain: []uint32{11}},
		{Chain: []uint32{12}},
	}},
}

const rfc5256ExampleLine = "* THREAD (2)(3 6 (4 23)(44 7 96))((11)(12))\r\n"

// threadSession answers every THREAD with a fixed tree, so the test knows what
// the server was given and can check what the client got back.
type threadSession struct {
	*imapmemserver.UserSession
	data []imap.ThreadData
}

func (s *threadSession) Thread(ctx context.Context, numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	return s.data, nil
}

// recordingConn keeps a copy of everything read from the server.
type recordingConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.buf.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

// line returns the first line read from the server that starts with prefix,
// CRLF included, or "" if there is none.
func (c *recordingConn) line(prefix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range strings.SplitAfter(c.buf.String(), "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// TestThreadRoundTrip sends a thread tree through the server's writer and the
// client's parser and checks that it comes back unchanged.
//
// The client's parser takes a missing SP before '(' in its stride, so on its
// own it cannot tell the RFC 5256 section 5 form from one with an SP between
// the thread-lists. Roundcube can, and drops every thread after such an SP.
// The test therefore also reads the THREAD line off the wire and checks it is
// in the RFC's form.
func TestThreadRoundTrip(t *testing.T) {
	user := newTestUser(t)
	server := newShutdownTestServer(t, func() imapserver.Session {
		return &threadSession{UserSession: imapmemserver.NewUserSession(user), data: rfc5256Example}
	}, imap.Cap("THREAD=REFERENCES"))

	raw, err := net.Dial("tcp", server.addr)
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	conn := &recordingConn{Conn: raw}
	client := imapclient.New(conn, nil)
	defer client.Close()

	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select() = %v", err)
	}

	got, err := client.UIDThread(&imapclient.ThreadOptions{
		Algorithm:      imap.ThreadReferences,
		SearchCriteria: &imap.SearchCriteria{},
	}).Wait()
	if err != nil {
		t.Fatalf("UIDThread().Wait() = %v", err)
	}
	if !reflect.DeepEqual(got, rfc5256Example) {
		t.Errorf("client read %+v, server sent %+v", got, rfc5256Example)
	}

	if line := conn.line("* THREAD"); line != rfc5256ExampleLine {
		t.Errorf("server wrote %q, want %q", line, rfc5256ExampleLine)
	}
}
