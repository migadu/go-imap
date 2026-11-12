package imapserver_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestAppendValidatesFramingBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		wantOK       bool
		wantCount    int
	}{
		{name: "complete command control", suffix: "\r\n", wantOK: true, wantCount: 1},
		{name: "invalid command terminator", suffix: "X\r\n", wantCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, user, closer := newNotifyTestServer(t)
			defer closer()
			if err := user.Create(context.Background(), "INBOX", nil); err != nil {
				t.Fatal(err)
			}
			c := dialNotifyTest(t, addr)
			defer c.close()
			c.login(t)
			body := "From: sender@example.com\r\nSubject: framing test\r\n\r\nHello\r\n"
			c.write(fmt.Sprintf("appendtest APPEND INBOX {%d}\r\n", len(body)))
			if line, err := c.readLine(3 * time.Second); err != nil || !strings.HasPrefix(line, "+") {
				t.Fatalf("continuation=%q err=%v", line, err)
			}
			c.write(body + tc.suffix)
			var response strings.Builder
			for i := 0; i < 10; i++ {
				line, err := c.readLine(3 * time.Second)
				response.WriteString(line)
				if strings.HasPrefix(line, "appendtest ") || strings.HasPrefix(line, "* BYE") {
					break
				}
				if err != nil {
					if tc.wantOK {
						t.Fatalf("response=%q err=%v", response.String(), err)
					}
					break
				}
			}
			if ok := strings.Contains(response.String(), "appendtest OK"); ok != tc.wantOK {
				t.Errorf("response=%q, want success=%v", response.String(), tc.wantOK)
			}
			data, err := user.Status(context.Background(), "INBOX", &imap.StatusOptions{NumMessages: true})
			if err != nil {
				t.Fatal(err)
			}
			if data.NumMessages == nil {
				t.Fatal("STATUS omitted message count")
			}
			if got := int(*data.NumMessages); got != tc.wantCount {
				t.Fatalf("APPEND response=%q but stored %d messages; want %d", response.String(), got, tc.wantCount)
			}
		})
	}
}

type appendFramingSession struct {
	imapserver.Session
	readFull bool
	commits  *atomic.Int64
}

func (s *appendFramingSession) Append(_ context.Context, _ string, r imap.LiteralReader, _ *imap.AppendOptions) (*imap.AppendData, error) {
	// A backend can trust Size and stop without an EOF read. In particular,
	// io.ReadFull ignores an error returned alongside the last requested byte.
	if s.readFull {
		if r.Size() > 0 {
			if n, err := r.Read(nil); n != 0 || err != nil {
				return nil, fmt.Errorf("empty read before body = %d, %v", n, err)
			}
		}
		if _, err := io.ReadFull(r, make([]byte, r.Size())); err != nil {
			return nil, err
		}
	} else if _, err := io.ReadAll(r); err != nil {
		return nil, err
	}
	s.commits.Add(1)
	return &imap.AppendData{}, nil
}

// The backend must not see a complete successful literal until the command's
// terminator has been validated. The zero-size ReadFull case never reads at all.
func TestAppendFramingBeforeBackendCommit(t *testing.T) {
	for _, format := range []struct {
		name, prefix, suffix, badSuffix string
	}{
		{"plain", "", "\r\n", "X\r\n"},
		{"binary", "~", "\r\n", "X\r\n"},
		{"utf8", "UTF8 (~", ")\r\n", ")X\r\n"},
		{"utf8 closing parenthesis", "UTF8 (~", ")\r\n", "X)\r\n"},
	} {
		for _, nonSync := range []bool{false, true} {
			for _, reader := range []struct {
				name, body string
				readFull   bool
			}{
				{"ReadAll", "hello", false},
				{"ReadFull", "hello", true},
				{"one byte ReadFull", "a", true},
				{"empty without Read", "", true},
			} {
				for _, valid := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/nonSync=%v/%s/valid=%v", format.name, nonSync, reader.name, valid), func(t *testing.T) {
						var commits atomic.Int64
						conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
							return &appendFramingSession{Session: session, readFull: reader.readFull, commits: &commits}
						})
						enableAppendUTF8(t, conn, br)
						modifier := ""
						if nonSync {
							modifier = "+"
						}
						if _, err := fmt.Fprintf(conn, "a1 APPEND INBOX %s{%d%s}\r\n", format.prefix, len(reader.body), modifier); err != nil {
							t.Fatal(err)
						}
						if !nonSync {
							if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
								t.Fatalf("continuation = %q, %v", line, err)
							}
						}
						suffix, status, wantCommits := format.badSuffix, "a1 BAD [CLIENTBUG]", int64(0)
						if valid {
							suffix, status, wantCommits = format.suffix, "a1 OK", 1
						}
						// Pipeline the next command to catch over-reading by either
						// the literal wrapper or the syntax-error recovery path.
						if _, err := io.WriteString(conn, reader.body+suffix+"a2 NOOP\r\n"); err != nil {
							t.Fatal(err)
						}
						if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, status) {
							t.Fatalf("APPEND = %q, %v; want %s", line, err, status)
						}
						if got := commits.Load(); got != wantCommits {
							t.Errorf("commits = %d; want %d", got, wantCommits)
						}
						if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "a2 OK") {
							t.Fatalf("pipelined NOOP = %q, %v", line, err)
						}
					})
				}
			}
		}
	}
}

type appendSuffixTimeoutSession struct {
	imapserver.Session
	conn    *appendDrainTimeoutConn
	commits *atomic.Int64
}

func (s *appendSuffixTimeoutSession) Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	// The announcement has been read, but the synchronizing upload has not
	// arrived yet. Expire the socket after its body is read, so only the
	// command suffix encounters a timeout.
	s.conn.armed.Store(true)
	return (&appendFramingSession{readFull: true, commits: s.commits}).Append(ctx, mailbox, r, options)
}

// Unlike the rejected-literal helper, this expires the deadline after receiving
// the body. This leaves buffered body bytes readable and makes the missing
// suffix fail on the next socket read.
type appendSuffixTimeoutConn struct{ *appendDrainTimeoutConn }

func (c *appendSuffixTimeoutConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.armed.Swap(false) {
		if deadlineErr := c.Conn.SetReadDeadline(time.Now().Add(-time.Second)); deadlineErr != nil {
			return n, deadlineErr
		}
	}
	return n, err
}

type appendSuffixTimeoutListener struct{ net.Listener }

func (l appendSuffixTimeoutListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &appendSuffixTimeoutConn{&appendDrainTimeoutConn{Conn: conn}}, nil
}

func TestAppendIncompleteSuffixDoesNotCommit(t *testing.T) {
	for _, format := range []struct{ name, prefix string }{{"plain", ""}, {"utf8", "UTF8 (~"}} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/timeout=%v", format.name, timeout), func(t *testing.T) {
				var commits atomic.Int64
				wrap := func(session imapserver.Session, conn *imapserver.Conn) imapserver.Session {
					if timeout {
						return &appendSuffixTimeoutSession{Session: session, conn: conn.NetConn().(*appendSuffixTimeoutConn).appendDrainTimeoutConn, commits: &commits}
					}
					return &appendFramingSession{Session: session, readFull: true, commits: &commits}
				}
				var listeners []func(net.Listener) net.Listener
				if timeout {
					listeners = append(listeners, func(listener net.Listener) net.Listener { return appendSuffixTimeoutListener{listener} })
				}
				conn, br := dialAppendRecoveryServer(t, wrap, listeners...)
				enableAppendUTF8(t, conn, br)
				if _, err := fmt.Fprintf(conn, "a1 APPEND INBOX %s{5}\r\n", format.prefix); err != nil {
					t.Fatal(err)
				}
				if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
					t.Fatalf("continuation = %q, %v", line, err)
				}
				if _, err := io.WriteString(conn, "hello"); err != nil {
					t.Fatal(err)
				}
				if !timeout {
					if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
						t.Fatal(err)
					}
				}
				defer func() {
					if got := commits.Load(); got != 0 {
						t.Errorf("incomplete APPEND suffix committed %d messages", got)
					}
				}()
				assertAppendTimeoutClosed(t, br)
			})
		}
	}
}
