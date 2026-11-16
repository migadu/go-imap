package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 5161 §3.1, RFC 9051 §6.3.1 & §7.2.1, RFC 8437 §2: handleEnabled must not
// scan past a pending authentication/session barrier (LOGIN, AUTHENTICATE,
// UNAUTHENTICATE, LOGOUT) in c.pendingCmds.
func TestEnableStopsAtPendingAuthBarriers(t *testing.T) {
	t.Run("rfc_compliant_cross_user_enable_leak", func(t *testing.T) {
		const aliceCaps = "IMAP4rev1 ENABLE UTF8=ACCEPT UNAUTHENTICATE AUTH=PLAIN"
		const bobCaps = "IMAP4rev1 ENABLE"

		c, done := newSelectIsolation307Peer(t, aliceCaps, nil, func(br *bufio.Reader, conn net.Conn) error {
			enableATag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			unauthTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			loginTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			enableBTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}

			if _, err := fmt.Fprintf(conn, "* ENABLED UTF8=ACCEPT\r\n%s OK enabled for alice\r\n", enableATag); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", unauthTag, aliceCaps); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in as bob\r\n", loginTag, bobCaps); err != nil {
				return err
			}
			// RFC 5161 §3.1 / RFC 9051 §6.3.1: unsupported ENABLE arguments
			// succeed with OK and an empty ENABLED response.
			if _, err := fmt.Fprintf(conn, "* ENABLED\r\n%s OK enable completed\r\n", enableBTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		enableA := c.Enable(imap.CapUTF8Accept)
		unauth := c.Unauthenticate()
		login := c.Login("bob", "secret")
		enableB := c.Enable(imap.CapUTF8Accept)

		dataA, err := enableA.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if !dataA.Caps.Has(imap.CapUTF8Accept) {
			t.Fatalf("Alice EnableData.Caps = %v, want UTF8=ACCEPT", dataA.Caps)
		}
		if err := unauth.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := login.Wait(); err != nil {
			t.Fatal(err)
		}
		dataB, err := enableB.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if len(dataB.Caps) != 0 {
			t.Fatalf("Bob's Enable stole Alice's pre-UNAUTHENTICATE ENABLED across UNAUTHENTICATE+LOGIN: got %v, want empty", dataB.Caps)
		}
	})

	t.Run("unrequested_pre_unauthenticate_enabled_preserves_mutf7_and_does_not_leak", func(t *testing.T) {
		const caps = "IMAP4rev1 ENABLE UTF8=ACCEPT UNAUTHENTICATE AUTH=PLAIN"
		var listedMailboxes []string

		c, done := newSelectIsolation307Peer(t, caps, &imapclient.Options{
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				List: func(d *imap.ListData) {
					listedMailboxes = append(listedMailboxes, d.Mailbox)
				},
			},
		}, func(br *bufio.Reader, conn net.Conn) error {
			unauthTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			loginTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			enableBTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}

			if _, err := fmt.Fprint(conn,
				"* ENABLED UTF8=ACCEPT\r\n"+
					"* LIST () \"/\" \"caf&AOk-\"\r\n"); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", unauthTag, caps); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY IMAP4rev1 ENABLE] logged in as bob\r\n", loginTag); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "* ENABLED\r\n%s OK enable completed\r\n", enableBTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		unauth := c.Unauthenticate()
		login := c.Login("bob", "secret")
		enableB := c.Enable(imap.CapUTF8Accept)

		if err := unauth.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := login.Wait(); err != nil {
			t.Fatal(err)
		}
		dataB, err := enableB.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if len(listedMailboxes) != 1 || listedMailboxes[0] != "café" {
			t.Errorf("Alice's pre-UNAUTHENTICATE MUTF-7 LIST decoded as %q, want [\"café\"] (unrequested ENABLED mutated decoder QuotedUTF8)", listedMailboxes)
		}
		if len(dataB.Caps) != 0 {
			t.Fatalf("Bob's Enable stole unrequested pre-UNAUTHENTICATE ENABLED: got %v, want empty", dataB.Caps)
		}
	})

	t.Run("unrequested_pre_logout_enabled_preserves_mutf7_and_does_not_leak", func(t *testing.T) {
		const caps = "IMAP4rev1 ENABLE UTF8=ACCEPT"
		var listedMailboxes []string

		c, done := newSelectIsolation307Peer(t, caps, &imapclient.Options{
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				List: func(d *imap.ListData) {
					listedMailboxes = append(listedMailboxes, d.Mailbox)
				},
			},
		}, func(br *bufio.Reader, conn net.Conn) error {
			logoutTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			enableTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}

			if _, err := fmt.Fprintf(conn,
				"* ENABLED UTF8=ACCEPT\r\n"+
					"* LIST () \"/\" \"caf&AOk-\"\r\n"+
					"* BYE logging out\r\n"+
					"%s OK logout\r\n"+
					"%s OK completed\r\n", logoutTag, enableTag); err != nil {
				return err
			}
			return nil
		})

		logout := c.Logout()
		enable := c.Enable(imap.CapUTF8Accept)

		if err := logout.Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := enable.Wait()
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if len(listedMailboxes) != 1 || listedMailboxes[0] != "café" {
			t.Errorf("pre-LOGOUT MUTF-7 LIST decoded as %q, want [\"café\"] (unrequested ENABLED mutated decoder QuotedUTF8)", listedMailboxes)
		}
		if len(data.Caps) != 0 {
			t.Fatalf("post-LOGOUT Enable stole pre-LOGOUT ENABLED: got %v, want empty", data.Caps)
		}
	})

	t.Run("prior_enabled_echo_does_not_leak_into_post_barrier_enable", func(t *testing.T) {
		const caps = "IMAP4rev1 ENABLE QRESYNC METADATA UNAUTHENTICATE"

		c, done := newSelectIsolation307Peer(t, caps, nil, func(br *bufio.Reader, conn net.Conn) error {
			firstTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "* ENABLED QRESYNC CONDSTORE\r\n%s OK enabled\r\n", firstTag); err != nil {
				return err
			}

			unauthTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			loginTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			enableBTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}

			if _, err := fmt.Fprint(conn, "* ENABLED QRESYNC\r\n"); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", unauthTag, caps); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in as bob\r\n", loginTag, caps); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(conn, "* ENABLED METADATA\r\n%s OK enabled\r\n", enableBTag); err != nil {
				return err
			}
			return finishSelectIsolation307Noop(br, conn)
		})

		if _, err := c.Enable(imap.CapQResync).Wait(); err != nil {
			t.Fatal(err)
		}
		unauth := c.Unauthenticate()
		login := c.Login("bob", "secret")
		enableB := c.Enable(imap.CapMetadata)

		if err := unauth.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := login.Wait(); err != nil {
			t.Fatal(err)
		}
		dataB, err := enableB.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if dataB.Caps.Has(imap.CapQResync) || len(dataB.Caps) != 1 || !dataB.Caps.Has(imap.CapMetadata) {
			t.Fatalf("Bob's Enable(METADATA) received Alice's prior QRESYNC echo across UNAUTHENTICATE+LOGIN: got %v, want only METADATA", dataB.Caps)
		}
	})
}

// handleID and handleCapability must also stop at pending authentication and
// session barriers so pre-barrier * ID and * CAPABILITY responses cannot leak
// into post-barrier IDCommand or CapabilityCommand results.
func TestIDAndCapabilityStopAtPendingAuthBarriers(t *testing.T) {
	for _, barrier := range []string{"LOGIN", "UNAUTHENTICATE_LOGIN", "LOGOUT"} {
		for _, kind := range []string{"ID", "Capability"} {
			t.Run(barrier+"/"+kind, func(t *testing.T) {
				const caps = "IMAP4rev2 ID UNAUTHENTICATE"
				serve := func(br *bufio.Reader, conn net.Conn) error {
					var unauthTag string
					if barrier == "UNAUTHENTICATE_LOGIN" {
						var err error
						unauthTag, err = readSelectIsolation307Command(br)
						if err != nil {
							return err
						}
					}
					barrierTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					cmdTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}

					if kind == "ID" {
						if _, err := fmt.Fprint(conn, "* ID (\"name\" \"AliceShard\" \"environment\" \"secret-tenant\")\r\n"); err != nil {
							return err
						}
					} else {
						if _, err := fmt.Fprint(conn, "* CAPABILITY IMAP4rev2 ID UNAUTHENTICATE X-ALICE-ADMIN\r\n"); err != nil {
							return err
						}
					}

					switch barrier {
					case "LOGIN":
						if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in\r\n%s OK [CAPABILITY %s] completed\r\n", barrierTag, caps, cmdTag, caps); err != nil {
							return err
						}
					case "UNAUTHENTICATE_LOGIN":
						if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n%s OK [CAPABILITY %s] logged in as bob\r\n%s OK [CAPABILITY %s] completed\r\n",
							unauthTag, caps, barrierTag, caps, cmdTag, caps); err != nil {
							return err
						}
					case "LOGOUT":
						if _, err := fmt.Fprintf(conn, "* BYE logging out\r\n%s OK logout\r\n%s OK completed\r\n", barrierTag, cmdTag); err != nil {
							return err
						}
					}

					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return nil
						}
						tTag, _, _ := strings.Cut(strings.TrimSpace(line), " ")
						if _, err := fmt.Fprintf(conn, "%s OK done\r\n", tTag); err != nil {
							return err
						}
					}
				}

				var (
					c    *imapclient.Client
					done <-chan error
				)
				if barrier == "LOGIN" {
					c, done = newUnauthBarrierPeer(t, caps, nil, serve)
					if err := c.WaitGreeting(); err != nil {
						t.Fatal(err)
					}
				} else {
					c, done = newSelectIsolation307Peer(t, caps, nil, serve)
				}

				var unauthCmd, barrierCmd *imapclient.Command
				switch barrier {
				case "LOGIN":
					barrierCmd = c.Login("alice", "secret")
				case "UNAUTHENTICATE_LOGIN":
					unauthCmd = c.Unauthenticate()
					barrierCmd = c.Login("bob", "secret")
				case "LOGOUT":
					barrierCmd = c.Logout()
				}

				if kind == "ID" {
					cmd := c.ID(&imap.IDData{Name: "go-imap"})
					if unauthCmd != nil {
						if err := unauthCmd.Wait(); err != nil {
							t.Fatal(err)
						}
					}
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					c.Close()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if data.Name != "" || len(data.Raw) != 0 {
						t.Fatalf("post-%s IDCommand stole pre-barrier ID data: %+v", barrier, data)
					}
				} else {
					cmd := c.Capability()
					if unauthCmd != nil {
						if err := unauthCmd.Wait(); err != nil {
							t.Fatal(err)
						}
					}
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					gotCaps, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					c.Close()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if gotCaps.Has(imap.Cap("X-ALICE-ADMIN")) {
						t.Fatalf("post-%s CapabilityCommand stole pre-barrier capabilities: %v", barrier, gotCaps)
					}
				}
			})
		}
	}
}
