package imapserver_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type glitchAfterPrefixConn struct {
	net.Conn
	armed    *atomic.Bool
	glitched atomic.Bool
}

func (c *glitchAfterPrefixConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.armed.Load() && c.glitched.CompareAndSwap(false, true) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

type glitchAfterPrefixListener struct {
	net.Listener
	armed *atomic.Bool
}

func (ln glitchAfterPrefixListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &glitchAfterPrefixConn{Conn: conn, armed: ln.armed}, nil
}

func TestIdleMidReadNonEOFErrorClosesWithoutServerBugOrSmuggling(t *testing.T) {
	t.Run("mid-IDLE non-EOF read error closes without SERVERBUG or smuggling", func(t *testing.T) {
		var armed atomic.Bool
		w := dialFramingIdleServer(t, func(ln net.Listener) net.Listener {
			return glitchAfterPrefixListener{Listener: ln, armed: &armed}
		})
		_ = w.conn.SetDeadline(time.Now().Add(3 * time.Second))

		fmt.Fprint(w.conn, "a2 IDLE\r\n")
		if cont := w.line(); !strings.HasPrefix(cont, "+ ") {
			t.Fatalf("IDLE continuation = %q, want '+ '", cont)
		}

		armed.Store(true)
		if _, err := io.WriteString(w.conn, "NOTDONE "); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w.conn, "x1 CREATE smuggled-mailbox\r\n")
		_ = w.conn.(*net.TCPConn).CloseWrite()

		raw, err := io.ReadAll(w.rd)
		if err != nil {
			t.Fatalf("read: %v (output %q)", err, raw)
		}
		out := string(raw)
		if strings.Contains(out, "SERVERBUG") {
			t.Fatalf("mid-IDLE client read error answered with SERVERBUG:\n%s", out)
		}
		if strings.Contains(out, "x1 ") {
			t.Fatalf("remainder of unfinished IDLE line executed as smuggled command:\n%s", out)
		}
	})

	t.Run("TLS truncated record mid-IDLE closes without SERVERBUG", func(t *testing.T) {
		mem := imapmemserver.New()
		u := imapmemserver.NewUser("user1", "pass1")
		if err := u.Create(context.Background(), "INBOX", nil); err != nil {
			t.Fatal(err)
		}
		mem.AddUser(u)

		tlsCfg := &tls.Config{Certificates: []tls.Certificate{debugTestCert(t)}}
		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return mem.NewSession(), nil, nil
			},
			TLSConfig: tlsCfg,
			Caps:      imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}},
			Logger:    log.New(io.Discard, "", 0),
		})
		rawLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		tlsLn := tls.NewListener(rawLn, tlsCfg)
		t.Cleanup(func() {
			tlsLn.Close()
			srv.Close()
		})
		go srv.Serve(tlsLn)

		rawConn, err := net.Dial("tcp", rawLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rawConn.Close() })
		_ = rawConn.SetDeadline(time.Now().Add(3 * time.Second))

		tlsClient := tls.Client(rawConn, &tls.Config{InsecureSkipVerify: true})
		if err := tlsClient.Handshake(); err != nil {
			t.Fatal(err)
		}
		rd := bufio.NewReader(tlsClient)
		if _, err := rd.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(tlsClient, "a1 LOGIN user1 pass1\r\n")
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "a1 ") {
				break
			}
		}
		fmt.Fprint(tlsClient, "a2 IDLE\r\n")
		cont, err := rd.ReadString('\n')
		if err != nil || !strings.HasPrefix(cont, "+ ") {
			t.Fatalf("IDLE continuation = %q, %v", cont, err)
		}

		if _, err := rawConn.Write([]byte{0x17, 0x03, 0x03, 0x00, 0x20, 'D', 'O'}); err != nil {
			t.Fatal(err)
		}
		_ = rawConn.(*net.TCPConn).CloseWrite()

		rest, readErr := io.ReadAll(rd)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			_ = readErr
		}
		if strings.Contains(string(rest), "a2 NO [SERVERBUG]") {
			t.Fatalf("truncated TLS record mid-IDLE answered with tagged SERVERBUG:\n%s", rest)
		}
	})
}
