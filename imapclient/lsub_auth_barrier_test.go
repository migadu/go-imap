package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/emersion/go-sasl"

	"github.com/migadu/go-imap"
)

// Mailbox case sensitivity is server-defined (RFC 3501 section 5.1).
// A case-insensitive server can preserve the canonical spelling of a
// subscription, so an LSUB response need not pass the client's matcher.
// Queuing a later authentication command must preserve the fallback that
// delivers such a response to the preceding LSUB.
func TestLsubPrecedingAuthBarrierKeepsFallback(t *testing.T) {
	for _, barrier := range []string{"LOGIN", "AUTHENTICATE", "UNAUTHENTICATE", "LOGOUT"} {
		for _, ref := range []string{"archive/", "ARCHIVE/", "Archive/"} {
			t.Run(barrier+"/"+ref, func(t *testing.T) {
				const caps = "IMAP4rev1 UNAUTHENTICATE AUTH=PLAIN SASL-IR"
				c, done := newSelectIsolation307Peer(t, caps, nil, func(br *bufio.Reader, conn net.Conn) error {
					lsubTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					barrierTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					if _, err := fmt.Fprintf(conn, "* LSUB () \"/\" \"Archive/Child\"\r\n%s OK listed\r\n", lsubTag); err != nil {
						return err
					}
					switch barrier {
					case "LOGIN", "AUTHENTICATE":
						// These commands cannot reauthenticate an authenticated
						// session, but still must not erase the earlier result.
						_, err = fmt.Fprintf(conn, "%s BAD already authenticated\r\n", barrierTag)
					case "UNAUTHENTICATE":
						_, err = fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", barrierTag, caps)
					case "LOGOUT":
						_, err = fmt.Fprintf(conn, "* BYE logging out\r\n%s OK logged out\r\n", barrierTag)
					}
					return err
				})

				lsub := c.Lsub(ref, "%")
				var waitBarrier func() error
				switch barrier {
				case "LOGIN":
					waitBarrier = c.Login("alice", "secret").Wait
				case "AUTHENTICATE":
					authDone := make(chan error, 1)
					go func() { authDone <- c.Authenticate(sasl.NewPlainClient("", "alice", "secret")) }()
					waitBarrier = func() error { return <-authDone }
				case "UNAUTHENTICATE":
					waitBarrier = c.Unauthenticate().Wait
				case "LOGOUT":
					waitBarrier = c.Logout().Wait
				}

				data, err := lsub.Collect()
				if err != nil {
					t.Fatal(err)
				}
				err = waitBarrier()
				wantState := imap.ConnStateAuthenticated
				if barrier == "LOGIN" || barrier == "AUTHENTICATE" {
					var imapErr *imap.Error
					if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBad {
						t.Fatalf("%s = %v, want BAD", barrier, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					wantState = imap.ConnStateNotAuthenticated
					if barrier == "LOGOUT" {
						wantState = imap.ConnStateLogout
					}
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if got := c.State(); got != wantState {
					t.Errorf("state = %v, want %v", got, wantState)
				}
				if len(data) != 1 || data[0].Mailbox != "Archive/Child" {
					t.Fatalf("preceding LSUB lost canonical-spelling response: got %+v, want Archive/Child", data)
				}
			})
		}
	}
}

// When there is no preceding LSUB, the fallback must not skip an
// authentication barrier and feed an old session's response to a later LSUB.
func TestLsubFallbackStopsAtAuthBarrier(t *testing.T) {
	for _, ref := range []string{"archive/", "ARCHIVE/", "Archive/"} {
		t.Run(ref, func(t *testing.T) {
			const caps = "IMAP4rev1 UNAUTHENTICATE"
			c, done := newSelectIsolation307Peer(t, caps, nil, func(br *bufio.Reader, conn net.Conn) error {
				unauthTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				loginTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				lsubTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, "* LSUB () \"/\" \"Archive/Child\"\r\n%s OK [CAPABILITY %s] unauthenticated\r\n%s OK [CAPABILITY %s] logged in\r\n%s OK listed\r\n", unauthTag, caps, loginTag, caps, lsubTag)
				return err
			})
			unauth := c.Unauthenticate()
			login := c.Login("bob", "secret")
			lsub := c.Lsub(ref, "%")
			if err := unauth.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := login.Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := lsub.Collect()
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if len(data) != 0 {
				t.Fatalf("later LSUB received old session's response: %+v", data)
			}
		})
	}
}
