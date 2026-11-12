package imapserver_test

import (
	"bufio"
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
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type appendTimeoutSession struct {
	imapserver.Session
	conn  *imapserver.Conn
	drain bool
}

func (s *appendTimeoutSession) Append(_ context.Context, _ string, r imap.LiteralReader, _ *imap.AppendOptions) (*imap.AppendData, error) {
	if _, err := io.ReadFull(r, make([]byte, 5)); err != nil {
		return nil, err
	}
	// Simulate the normal literal read timeout without waiting five minutes.
	if err := s.conn.NetConn().SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		return nil, err
	}
	if s.drain {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "backend refused APPEND"}
	}
	_, err := r.Read(make([]byte, 1))
	return nil, err
}

func (s *appendTimeoutSession) AppendCatenate(ctx context.Context, mailbox string, parts imapserver.CatenateReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	part, err := parts.Next()
	if err != nil {
		return nil, err
	}
	return s.Append(ctx, mailbox, part.Text, options)
}

func dialAppendRecoveryServer(t *testing.T, wrap func(imapserver.Session, *imapserver.Conn) imapserver.Session, wrapListener ...func(net.Listener) net.Listener) (net.Conn, *bufio.Reader) {
	t.Helper()
	user := imapmemserver.NewUser("user", "pass")
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return wrap(imapmemserver.NewUserSession(user), conn), &imapserver.GreetingData{PreAuth: true}, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapLiteralPlus: {}, imap.CapBinary: {}, imap.CapUTF8Accept: {}, imap.CapCatenate: {}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var listener net.Listener = ln
	if len(wrapListener) != 0 {
		listener = wrapListener[0](listener)
	}
	go srv.Serve(listener)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); srv.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* PREAUTH") {
		t.Fatalf("greeting = %q, %v", line, err)
	}
	return conn, br
}

func TestAppendTimeoutClosesConnection(t *testing.T) {
	for _, format := range []string{"plain", "binary", "utf8", "catenate"} {
		for _, nonSync := range []bool{false, true} {
			for _, drain := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/nonSync=%v/finalDrain=%v", format, nonSync, drain), func(t *testing.T) {
					conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, conn *imapserver.Conn) imapserver.Session {
						return &appendTimeoutSession{Session: session, conn: conn, drain: drain}
					})
					announcement := "{14}"
					if nonSync {
						announcement = "{14+}"
					}
					switch format {
					case "binary":
						announcement = "~" + announcement
					case "utf8":
						announcement = "UTF8 (~" + announcement
					case "catenate":
						announcement = "CATENATE (TEXT " + announcement
					}
					if _, err := fmt.Fprintf(conn, "a1 APPEND INBOX %s\r\n", announcement); err != nil {
						t.Fatal(err)
					}
					if !nonSync {
						if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
							t.Fatalf("continuation = %q, %v", line, err)
						}
					}
					if _, err := io.WriteString(conn, "hello"); err != nil {
						t.Fatal(err)
					}
					assertAppendTimeoutClosed(t, br)
				})
			}
		}
	}
}

type appendRefusalSession struct{ imapserver.Session }

func (s *appendRefusalSession) Append(context.Context, string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "backend refused APPEND"}
}

// A backend refusal alone must not close a well-framed connection. The handler
// drains its unread body before reporting NO, then processes the next command.
func TestAppendRefusalAfterSuccessfulDrainKeepsConnection(t *testing.T) {
	for _, nonSync := range []bool{false, true} {
		t.Run(fmt.Sprint(nonSync), func(t *testing.T) {
			conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
				return &appendRefusalSession{Session: session}
			})
			announcement := "{5}"
			if nonSync {
				announcement = "{5+}"
			}
			if _, err := fmt.Fprintf(conn, "a1 APPEND INBOX %s\r\n", announcement); err != nil {
				t.Fatal(err)
			}
			if !nonSync {
				if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
					t.Fatalf("continuation = %q, %v", line, err)
				}
			}
			if _, err := io.WriteString(conn, "hello\r\n"); err != nil {
				t.Fatal(err)
			}
			if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "a1 NO ") || strings.Contains(line, "SERVERBUG") {
				t.Fatalf("APPEND refusal = %q, %v", line, err)
			}
			if _, err := io.WriteString(conn, "a2 NOOP\r\n"); err != nil {
				t.Fatal(err)
			}
			if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "a2 OK ") {
				t.Fatalf("NOOP after drained refusal = %q, %v", line, err)
			}
		})
	}
}

type appendDrainTimeoutConn struct {
	net.Conn
	armed atomic.Bool
}

func (c *appendDrainTimeoutConn) Read(p []byte) (int, error) {
	// Expire the actual socket only once the rejected-literal drain starts
	// reading, after it has installed its normal literal timeout.
	if c.armed.Swap(false) {
		if err := c.Conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

type appendDrainTimeoutListener struct{ net.Listener }

func (l appendDrainTimeoutListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &appendDrainTimeoutConn{Conn: conn}, nil
}

type appendRejectedTimeoutSession struct {
	imapserver.Session
	conn   *appendDrainTimeoutConn
	called *atomic.Bool
}

func (s *appendRejectedTimeoutSession) AppendLimit() uint32 {
	s.conn.armed.Store(true)
	return 4
}

func (s *appendRejectedTimeoutSession) Append(context.Context, string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	s.called.Store(true)
	return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "unexpected backend call"}
}

func TestAppendRejectedNonSyncDrainTimeoutClosesConnection(t *testing.T) {
	var called atomic.Bool
	conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, conn *imapserver.Conn) imapserver.Session {
		return &appendRejectedTimeoutSession{Session: session, conn: conn.NetConn().(*appendDrainTimeoutConn), called: &called}
	}, func(listener net.Listener) net.Listener { return appendDrainTimeoutListener{listener} })
	if _, err := io.WriteString(conn, "a1 APPEND INBOX {14+}\r\n"); err != nil {
		t.Fatal(err)
	}
	assertAppendTimeoutClosed(t, br)
	if called.Load() {
		t.Fatal("oversized APPEND reached the backend")
	}
}

type appendSmallLimitSession struct{ imapserver.Session }

func (*appendSmallLimitSession) AppendLimit() uint32 { return 4 }

func TestAppendRejectedNonSyncMalformedTerminatorClosesConnection(t *testing.T) {
	for _, suffix := range []string{"x\r\n", "\rX"} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			conn, br := dialAppendRecoveryServer(t, func(session imapserver.Session, _ *imapserver.Conn) imapserver.Session {
				return &appendSmallLimitSession{Session: session}
			})
			if _, err := io.WriteString(conn, "a1 APPEND INBOX {5+}\r\nhello"+suffix); err != nil {
				t.Fatal(err)
			}
			assertAppendTimeoutClosed(t, br)
		})
	}
}

func assertAppendTimeoutClosed(t *testing.T, br *bufio.Reader) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("connection stayed open after incomplete APPEND: %v", err)
			}
			return
		}
		if !strings.HasPrefix(line, "* BYE") {
			t.Fatalf("incomplete APPEND must close the connection, got %q", line)
		}
	}
}
