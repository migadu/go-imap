package imapclient

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
)

func TestLogoutCompletionRetainsUTF8SessionReset(t *testing.T) {
	const dualCaps = "IMAP4rev1 IMAP4rev2 ENABLE UTF8=ACCEPT LITERAL+ MULTISEARCH"
	for _, enableCap := range []imap.Cap{imap.CapUTF8Accept, imap.CapIMAP4rev2} {
		t.Run(string(enableCap), func(t *testing.T) {
			cli, srv := net.Pipe()
			defer cli.Close()
			defer srv.Close()
			_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
			_ = srv.SetDeadline(time.Now().Add(5 * time.Second))

			wire := make(chan string, 5)
			releaseLogout := make(chan struct{})
			releaseEOF := make(chan struct{})
			done := make(chan error, 1)

			go func() {
				br := bufio.NewReader(srv)
				readCmd := func() (tag, rest string, err error) {
					line, err := br.ReadString('\n')
					if err != nil {
						return "", "", err
					}
					tag, rest, _ = strings.Cut(line, " ")
					if strings.HasSuffix(rest, "{5+}\r\n") {
						lit := make([]byte, 7) // 5 bytes + CRLF
						if _, err := io.ReadFull(br, lit); err != nil {
							return "", "", err
						}
						rest += string(lit)
					}
					return tag, rest, nil
				}

				if _, err := fmt.Fprintf(srv, "* PREAUTH [CAPABILITY %s] ready\r\n", dualCaps); err != nil {
					done <- err
					return
				}

				enableTag, _, err := readCmd()
				if err != nil {
					done <- err
					return
				}
				if _, err := fmt.Fprintf(srv, "* ENABLED %s\r\n%s OK enabled\r\n", enableCap, enableTag); err != nil {
					done <- err
					return
				}

				logoutTag, _, err := readCmd()
				if err != nil {
					done <- err
					return
				}
				inFlightDelTag, inFlightDelRest, err := readCmd()
				if err != nil {
					done <- err
					return
				}
				wire <- inFlightDelRest

				<-releaseLogout
				if _, err := fmt.Fprintf(srv, "* BYE logging out\r\n%s OK logout\r\n%s BAD logged out\r\n", logoutTag, inFlightDelTag); err != nil {
					done <- err
					return
				}

				for i := 0; i < 4; i++ {
					tag, rest, err := readCmd()
					if err != nil {
						done <- err
						return
					}
					wire <- rest
					if _, err := fmt.Fprintf(srv, "%s BAD already logged out\r\n", tag); err != nil {
						done <- err
						return
					}
				}

				<-releaseEOF
				_ = srv.Close()
				done <- nil
			}()

			c := New(cli, &Options{ResponseTimeout: 4 * time.Second})
			defer c.Close()

			if _, err := c.Enable(enableCap).Wait(); err != nil {
				t.Fatalf("Enable(%s): %v", enableCap, err)
			}

			logout := c.Logout()
			inFlightDel := c.Delete("A&-B")
			close(releaseLogout)

			if err := logout.Wait(); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			_ = inFlightDel.Wait()

			c.mutex.Lock()
			decQuotedUTF8 := c.dec.QuotedUTF8
			c.mutex.Unlock()
			if decQuotedUTF8 {
				t.Errorf("after Logout() completed, decoder QuotedUTF8 remained true from terminated session's ENABLE")
			}

			_ = c.Login("bob", "päss").Wait()
			_ = c.Delete("A&-B").Wait()
			_, _ = c.UIDSearch(&imap.SearchCriteria{Body: []string{"café"}}, nil).Wait()
			_, _ = c.MultiSearch(&imap.SearchSource{Mailboxes: []string{"A&-B"}}, &imap.SearchCriteria{Body: []string{"café"}}, nil).Wait()

			close(releaseEOF)
			if err := <-done; err != nil {
				t.Fatalf("server: %v", err)
			}

			gotInFlightDelete := <-wire
			gotLogin := <-wire
			gotPostDelete := <-wire
			gotSearch := <-wire
			gotMultiSearch := <-wire

			if gotInFlightDelete != "DELETE \"A&--B\"\r\n" {
				t.Fatalf("while LOGOUT was pending, DELETE = %q, want MUTF-7 %q", gotInFlightDelete, "DELETE \"A&--B\"\r\n")
			}
			if gotLogin != "LOGIN \"bob\" {5+}\r\npäss\r\n" && gotLogin != "LOGIN bob {5+}\r\npäss\r\n" {
				t.Errorf("after LOGOUT completed, LOGIN reverted to raw 8-bit UTF-8: %q", gotLogin)
			}
			if gotPostDelete != "DELETE \"A&--B\"\r\n" {
				t.Errorf("after LOGOUT completed, DELETE reverted from %q back to unescaped UTF-8 %q", gotInFlightDelete, gotPostDelete)
			}
			if gotSearch != "UID SEARCH CHARSET UTF-8 BODY {5+}\r\ncafé\r\n" {
				t.Errorf("after LOGOUT completed, UID SEARCH reverted to omitting CHARSET UTF-8: %q", gotSearch)
			}
			if gotMultiSearch != "ESEARCH IN (mailboxes \"A&--B\") CHARSET UTF-8 BODY {5+}\r\ncafé\r\n" {
				t.Errorf("after LOGOUT completed, ESEARCH reverted to prior session QuotedUTF8: %q", gotMultiSearch)
			}
		})
	}

	t.Run("PureIMAP4rev2", func(t *testing.T) {
		cli, srv := net.Pipe()
		defer cli.Close()
		defer srv.Close()
		_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
		_ = srv.SetDeadline(time.Now().Add(5 * time.Second))

		releaseEOF := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			br := bufio.NewReader(srv)
			if _, err := fmt.Fprintf(srv, "* PREAUTH [CAPABILITY IMAP4rev2 LITERAL+] ready\r\n"); err != nil {
				done <- err
				return
			}
			line, err := br.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			if _, err := fmt.Fprintf(srv, "* BYE bye\r\n%s OK logout\r\n", tag); err != nil {
				done <- err
				return
			}
			<-releaseEOF
			_ = srv.Close()
			done <- nil
		}()

		c := New(cli, &Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()

		if err := c.Logout().Wait(); err != nil {
			t.Fatalf("Logout: %v", err)
		}
		c.mutex.Lock()
		decQuotedUTF8 := c.dec.QuotedUTF8
		encQuotedUTF8 := c.useEncoderQuotedUTF8Locked()
		c.mutex.Unlock()
		close(releaseEOF)
		if err := <-done; err != nil {
			t.Fatalf("server: %v", err)
		}
		if !decQuotedUTF8 || !encQuotedUTF8 {
			t.Errorf("pure IMAP4rev2 server lost QuotedUTF8 after Logout (dec=%v, enc=%v)", decQuotedUTF8, encQuotedUTF8)
		}
	})
}

type fragmentedAutoIdleConn struct {
	net.Conn
	prefix    string
	sawPrefix bool
	blocked   chan struct{}
	once      sync.Once
}

func (c *fragmentedAutoIdleConn) Read(p []byte) (int, error) {
	if c.sawPrefix {
		c.once.Do(func() { close(c.blocked) })
	}
	n, err := c.Conn.Read(p)
	if strings.HasSuffix(string(p[:n]), c.prefix) {
		c.sawPrefix = true
	}
	return n, err
}

func TestFragmentedSessionResetBlocksAutoIdle(t *testing.T) {
	t.Run("UnauthenticatePreservesAutoIdleForRelogin", func(t *testing.T) {
		const caps = "IMAP4rev1 IDLE UNAUTHENTICATE"
		cli, srv := net.Pipe()
		conn := &fragmentedAutoIdleConn{Conn: cli, prefix: "T1 OK ", blocked: make(chan struct{})}
		defer cli.Close()
		defer srv.Close()
		_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
		_ = srv.SetDeadline(time.Now().Add(5 * time.Second))

		spuriousIdle := make(chan string, 1)
		reloginIdle := make(chan struct{}, 1)
		unauthTextRelease := make(chan struct{})
		done := make(chan error, 1)

		go func() {
			br := bufio.NewReader(srv)
			if _, err := fmt.Fprintf(srv, "* PREAUTH [CAPABILITY %s] ready\r\n", caps); err != nil {
				done <- err
				return
			}
			line, err := br.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			unauthTag, _, _ := strings.Cut(line, " ")
			if _, err := fmt.Fprintf(srv, "%s OK ", unauthTag); err != nil {
				done <- err
				return
			}

			cmdCh := make(chan string, 1)
			go func() {
				next, err := br.ReadString('\n')
				if err == nil {
					cmdCh <- next
				}
			}()

			select {
			case next := <-cmdCh:
				idleTag, rest, _ := strings.Cut(next, " ")
				if rest == "IDLE\r\n" {
					spuriousIdle <- next
					_, _ = fmt.Fprintf(srv, "[CAPABILITY %s] unauthenticated\r\n%s BAD IDLE requires authenticated state\r\n", caps, idleTag)
					done <- nil
					return
				}
			case <-unauthTextRelease:
				if _, err := fmt.Fprintf(srv, "[CAPABILITY %s] unauthenticated\r\n", caps); err != nil {
					done <- err
					return
				}
				var idleTag string
				for {
					var next string
					if cmdCh != nil {
						next = <-cmdCh
						cmdCh = nil
					} else {
						var err error
						next, err = br.ReadString('\n')
						if err != nil {
							done <- nil
							return
						}
					}
					if strings.TrimSpace(next) == "DONE" && idleTag != "" {
						_, _ = fmt.Fprintf(srv, "%s OK idle terminated\r\n", idleTag)
						idleTag = ""
						continue
					}
					tag, rest, _ := strings.Cut(next, " ")
					switch {
					case strings.HasPrefix(rest, "LOGIN "):
						_, _ = fmt.Fprintf(srv, "%s OK [CAPABILITY %s] logged in\r\n", tag, caps)
					case rest == "IDLE\r\n":
						idleTag = tag
						_, _ = fmt.Fprintf(srv, "+ idling\r\n")
						select {
						case reloginIdle <- struct{}{}:
						default:
						}
					case rest == "NOOP\r\n":
						_, _ = fmt.Fprintf(srv, "%s OK noop\r\n", tag)
						done <- nil
						return
					}
				}
			}
		}()

		c := New(conn, &Options{
			AutoIdle:        15 * time.Millisecond,
			ResponseTimeout: 4 * time.Second,
		})
		defer c.Close()

		unauth := c.Unauthenticate()
		select {
		case <-conn.blocked:
		case <-time.After(3 * time.Second):
			t.Fatal("reader never awaited fragmented UNAUTHENTICATE text")
		}

		idleDone := make(chan struct{})
		go func() {
			defer close(idleDone)
			c.startAutoIdle()
		}()

		select {
		case got := <-spuriousIdle:
			<-idleDone
			_ = unauth.Wait()
			c.mutex.Lock()
			refused := c.autoIdleRefused
			c.mutex.Unlock()
			_ = <-done
			t.Fatalf("startAutoIdle sent %q during completingSessionReset window, setting autoIdleRefused=%v", got, refused)
		case <-idleDone:
			close(unauthTextRelease)
			if err := unauth.Wait(); err != nil {
				t.Fatalf("Unauthenticate: %v", err)
			}
			if err := c.Login("bob", "pass").Wait(); err != nil {
				t.Fatalf("Login after Unauthenticate: %v", err)
			}
			select {
			case <-reloginIdle:
			case <-time.After(time.Second):
				t.Fatal("AutoIdle did not resume after re-login")
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatalf("Noop after automatic IDLE: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("server: %v", err)
			}
		}
	})

	t.Run("LogoutBlocksAutoIdle", func(t *testing.T) {
		const caps = "IMAP4rev1 IDLE"
		cli, srv := net.Pipe()
		conn := &fragmentedAutoIdleConn{Conn: cli, prefix: "T1 OK ", blocked: make(chan struct{})}
		defer cli.Close()
		defer srv.Close()
		_ = cli.SetDeadline(time.Now().Add(5 * time.Second))
		_ = srv.SetDeadline(time.Now().Add(5 * time.Second))

		spuriousIdle := make(chan string, 1)
		logoutTextRelease := make(chan struct{})
		done := make(chan error, 1)

		go func() {
			br := bufio.NewReader(srv)
			if _, err := fmt.Fprintf(srv, "* PREAUTH [CAPABILITY %s] ready\r\n", caps); err != nil {
				done <- err
				return
			}
			line, err := br.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			logoutTag, _, _ := strings.Cut(line, " ")
			if _, err := fmt.Fprintf(srv, "* BYE bye\r\n%s OK ", logoutTag); err != nil {
				done <- err
				return
			}

			cmdCh := make(chan string, 1)
			go func() {
				next, err := br.ReadString('\n')
				if err == nil {
					cmdCh <- next
				}
			}()

			select {
			case next := <-cmdCh:
				idleTag, _, _ := strings.Cut(next, " ")
				spuriousIdle <- next
				_, _ = fmt.Fprintf(srv, "logout\r\n%s BAD already logged out\r\n", idleTag)
				_ = srv.Close()
				done <- nil
			case <-logoutTextRelease:
				_, _ = fmt.Fprintf(srv, "logout\r\n")
				_ = srv.Close()
				done <- nil
			}
		}()

		c := New(conn, &Options{
			AutoIdle:        50 * time.Millisecond,
			ResponseTimeout: 4 * time.Second,
		})
		defer c.Close()

		logout := c.Logout()
		select {
		case <-conn.blocked:
		case <-time.After(3 * time.Second):
			t.Fatal("reader never awaited fragmented LOGOUT text")
		}

		idleDone := make(chan struct{})
		go func() {
			defer close(idleDone)
			c.startAutoIdle()
		}()

		select {
		case got := <-spuriousIdle:
			<-idleDone
			_ = logout.Wait()
			_ = <-done
			t.Fatalf("startAutoIdle sent %q during fragmented LOGOUT completion", got)
		case <-idleDone:
			close(logoutTextRelease)
			if err := logout.Wait(); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("server: %v", err)
			}
		}
	})
}
