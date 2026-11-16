package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// Signal only when the reader asks for another socket read after consuming
// UNAUTHENTICATE's tagged "OK ". At that point readResponseTagged has already
// deleted its command from pendingCmds and is waiting for the response text.
type fragmentedResetConn struct {
	net.Conn
	sawPrefix bool
	blocked   chan struct{}
	once      sync.Once
}

func (c *fragmentedResetConn) Read(p []byte) (int, error) {
	if c.sawPrefix {
		c.once.Do(func() { close(c.blocked) })
	}
	n, err := c.Conn.Read(p)
	if string(p[:n]) == "T2 OK " {
		c.sawPrefix = true
	}
	return n, err
}

func TestFragmentedUnauthenticateRetainsEncoderReset(t *testing.T) {
	const caps = "IMAP4rev1 IMAP4rev2 ENABLE UTF8=ACCEPT UNAUTHENTICATE LITERAL+"
	for _, enableCap := range []imap.Cap{imap.CapUTF8Accept, imap.CapIMAP4rev2} {
		t.Run(string(enableCap), func(t *testing.T) {
			cli, srv := net.Pipe()
			conn := &fragmentedResetConn{Conn: cli, blocked: make(chan struct{})}
			defer cli.Close()
			defer srv.Close()
			cli.SetDeadline(time.Now().Add(5 * time.Second))
			srv.SetDeadline(time.Now().Add(5 * time.Second))
			done := make(chan error, 1)
			wire := make(chan string, 2)
			go func() {
				br := bufio.NewReader(srv)
				if _, err := fmt.Fprintf(srv, "* PREAUTH [CAPABILITY %s] ready\r\n", caps); err != nil {
					done <- err
					return
				}
				enable, err := br.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				enableTag, _, _ := strings.Cut(enable, " ")
				if _, err := fmt.Fprintf(srv, "* ENABLED %s\r\n%s OK enabled\r\n", enableCap, enableTag); err != nil {
					done <- err
					return
				}
				unauth, err := br.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				unauthTag, _, _ := strings.Cut(unauth, " ")
				if _, err := fmt.Fprintf(srv, "%s OK ", unauthTag); err != nil {
					done <- err
					return
				}
				login, err := br.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				loginTag, _, _ := strings.Cut(login, " ")
				if strings.HasSuffix(login, "{5+}\r\n") {
					rest := make([]byte, 7)
					if _, err := io.ReadFull(br, rest); err != nil {
						done <- err
						return
					}
					login += string(rest)
				}
				wire <- login
				del, err := br.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				delTag, _, _ := strings.Cut(del, " ")
				wire <- del
				if _, err := fmt.Fprintf(srv, "unauthenticated\r\n%s OK [CAPABILITY %s] logged in\r\n%s OK deleted\r\n", loginTag, caps, delTag); err != nil {
					done <- err
					return
				}
				for {
					next, err := br.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					nextTag, rest, _ := strings.Cut(next, " ")
					if rest == "CAPABILITY\r\n" {
						if _, err := fmt.Fprintf(srv, "* CAPABILITY %s\r\n%s OK capability\r\n", caps, nextTag); err != nil {
							done <- err
							return
						}
						continue
					}
					if rest != "NOOP\r\n" {
						done <- fmt.Errorf("unexpected command %q", next)
						return
					}
					_, err = fmt.Fprintf(srv, "%s OK done\r\n", nextTag)
					done <- err
					return
				}
			}()
			c := imapclient.New(conn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
			defer c.Close()
			if _, err := c.Enable(enableCap).Wait(); err != nil {
				t.Fatal(err)
			}
			unauth := c.Unauthenticate()
			select {
			case <-conn.blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("reader never awaited fragmented UNAUTHENTICATE text")
			}
			login := c.Login("bob", "päss")
			del := c.Delete("A&-B")
			if err := unauth.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := login.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := del.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			gotLogin, gotDelete := <-wire, <-wire
			t.Logf("LOGIN wire: %q; DELETE wire: %q", gotLogin, gotDelete)
			_, loginArgs, _ := strings.Cut(gotLogin, " ")
			if loginArgs != "LOGIN \"bob\" {5+}\r\npäss\r\n" {
				t.Errorf("post-reset LOGIN = %q, want password in an IMAP4rev1 literal", gotLogin)
			}
			_, deleteArgs, _ := strings.Cut(gotDelete, " ")
			if deleteArgs != "DELETE \"A&--B\"\r\n" {
				t.Errorf("post-reset DELETE wire = %q, want MUTF-7 A&--B (A&-B targets the distinct mailbox A&B)", gotDelete)
			}

		})
	}
}
