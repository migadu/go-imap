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

// ID and CAPABILITY are valid in either authentication state. Replies to
// commands preceding an authentication transition retain their owner, while
// later commands receive their own replies. CAPABILITY requires an untagged
// response before its tagged OK (RFC 9051 section 6.1.1); ID may answer NIL
// (RFC 2971 section 3.1). LOGOUT closes the connection, so only commands sent
// before it are answered in that case.
func TestIDAndCapabilityRepliesAroundAuthTransitions(t *testing.T) {
	for _, barrier := range []string{"LOGIN", "UNAUTHENTICATE_LOGIN", "LOGOUT"} {
		for _, kind := range []string{"ID", "Capability"} {
			t.Run(barrier+"/"+kind, func(t *testing.T) {
				const caps = "IMAP4rev2 ID UNAUTHENTICATE"
				const earlierCaps = caps + " X-EARLIER"
				const laterCaps = caps + " X-LATER"
				serve := func(br *bufio.Reader, conn net.Conn) error {
					earlierTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					var unauthTag string
					if barrier == "UNAUTHENTICATE_LOGIN" {
						unauthTag, err = readSelectIsolation307Command(br)
						if err != nil {
							return err
						}
					}
					barrierTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					var laterTag string
					if barrier != "LOGOUT" {
						laterTag, err = readSelectIsolation307Command(br)
						if err != nil {
							return err
						}
					}

					var earlierData string
					if kind == "ID" {
						earlierData = "* ID (\"name\" \"EarlierServer\" \"version\" \"1.0\")\r\n"
					} else {
						earlierData = "* CAPABILITY " + earlierCaps + "\r\n"
					}
					if _, err := fmt.Fprintf(conn, "%s%s OK earlier command completed\r\n", earlierData, earlierTag); err != nil {
						return err
					}
					if barrier == "LOGOUT" {
						_, err := fmt.Fprintf(conn, "* BYE logging out\r\n%s OK logout\r\n", barrierTag)
						return err
					}
					if unauthTag != "" {
						if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", unauthTag, caps); err != nil {
							return err
						}
					}
					if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in\r\n", barrierTag, laterCaps); err != nil {
						return err
					}
					var laterData string
					if kind == "ID" {
						laterData = "* ID NIL\r\n"
					} else {
						laterData = "* CAPABILITY " + laterCaps + "\r\n"
					}
					if _, err := fmt.Fprintf(conn, "%s%s OK later command completed\r\n", laterData, laterTag); err != nil {
						return err
					}
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return nil
						}
						tag, _, _ := strings.Cut(strings.TrimSpace(line), " ")
						if _, err := fmt.Fprintf(conn, "%s OK done\r\n", tag); err != nil {
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

				var earlierID, laterID *imapclient.IDCommand
				var earlierCapability, laterCapability *imapclient.CapabilityCommand
				if kind == "ID" {
					earlierID = c.ID(nil)
				} else {
					earlierCapability = c.Capability()
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
				if barrier != "LOGOUT" {
					if kind == "ID" {
						laterID = c.ID(nil)
					} else {
						laterCapability = c.Capability()
					}
				}

				if kind == "ID" {
					data, err := earlierID.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if data.Name != "EarlierServer" || data.Version != "1.0" {
						t.Errorf("earlier ID data = %+v, want EarlierServer version 1.0", data)
					}
				} else {
					data, err := earlierCapability.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data) != 4 || !data.Has(imap.Cap("X-EARLIER")) || data.Has(imap.Cap("X-LATER")) {
						t.Errorf("earlier CAPABILITY data = %v, want %s", data, earlierCaps)
					}
				}
				if unauthCmd != nil {
					if err := unauthCmd.Wait(); err != nil {
						t.Fatal(err)
					}
				}
				if err := barrierCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if laterID != nil {
					data, err := laterID.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if data.Name != "" || len(data.Raw) != 0 {
						t.Errorf("later ID data = %+v, want NIL response", data)
					}
				}
				if laterCapability != nil {
					data, err := laterCapability.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data) != 4 || !data.Has(imap.Cap("X-LATER")) || data.Has(imap.Cap("X-EARLIER")) {
						t.Errorf("later CAPABILITY data = %v, want %s", data, laterCaps)
					}
				}
				c.Close()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
