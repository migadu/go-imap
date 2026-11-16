package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 2971 section 3.1 and RFC 9051 section 6.1 allow ID and CAPABILITY in
// either authentication state. RFC 9051 section 5.5 permits independent
// commands to run concurrently. This peer's implementation identity and
// capability set are constant across LOGIN, so LOGIN cannot affect either
// result. Their untagged data can therefore arrive while LOGIN is pending,
// whether their own tagged completion arrives before or after LOGIN's.
func TestAnyStateCommandDataWhileLoginPending(t *testing.T) {
	const caps = "IMAP4rev2 ID AUTH=PLAIN"
	for _, kind := range []string{"ID", "CAPABILITY"} {
		for _, ownCompletionFirst := range []bool{true, false} {
			name := fmt.Sprintf("%s/own_completion_first=%v", kind, ownCompletionFirst)
			t.Run(name, func(t *testing.T) {
				c, done := newUnauthBarrierPeer(t, caps, nil, func(br *bufio.Reader, conn net.Conn) error {
					loginTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					commandTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}

					// There is only one outstanding command of this type, and
					// this is its requested response, not unsolicited data.
					var data string
					if kind == "ID" {
						data = "* ID (\"name\" \"FixedServer\" \"version\" \"1.0\")\r\n"
					} else {
						data = "* CAPABILITY " + caps + "\r\n"
					}
					loginCompletion := fmt.Sprintf("%s OK [CAPABILITY %s] authenticated\r\n", loginTag, caps)
					commandCompletion := fmt.Sprintf("%s OK %s completed\r\n", commandTag, kind)
					if ownCompletionFirst {
						data += commandCompletion + loginCompletion
					} else {
						data += loginCompletion + commandCompletion
					}
					if _, err := fmt.Fprint(conn, data); err != nil {
						return err
					}
					return finishSelectIsolation307Noop(br, conn)
				})
				if err := c.WaitGreeting(); err != nil {
					t.Fatal(err)
				}

				login := c.Login("alice", "secret")
				var (
					idCommand         *imapclient.IDCommand
					capabilityCommand *imapclient.CapabilityCommand
				)
				if kind == "ID" {
					idCommand = c.ID(nil)
				} else {
					capabilityCommand = c.Capability()
				}
				if err := login.Wait(); err != nil {
					t.Fatal(err)
				}

				if kind == "ID" {
					data, err := idCommand.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if data.Name != "FixedServer" || data.Version != "1.0" {
						t.Errorf("ID data = %+v, want FixedServer version 1.0", data)
					}
				} else {
					data, err := capabilityCommand.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data) != 3 || !data.Has(imap.CapIMAP4rev2) || !data.Has(imap.CapID) || !data.Has(imap.Cap("AUTH=PLAIN")) {
						t.Errorf("CAPABILITY data = %v, want %s", data, caps)
					}
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
