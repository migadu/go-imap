package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestAutoIdleCapabilityTransitions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		preauth       bool
		untaggedLogin bool
		wantFirstIdle string
	}{
		{name: "PREAUTH with initial capability refresh", preauth: true, wantFirstIdle: "CAPABILITY IDLE"},
		{name: "LOGIN with capability refresh", wantFirstIdle: "LOGIN CAPABILITY IDLE"},
		{name: "LOGIN with untagged capabilities", untaggedLogin: true, wantFirstIdle: "LOGIN IDLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
			capsRequested := make(chan struct{}, 1)
			releaseCaps := make(chan struct{})
			srv := &autoIdleServer{conn: serverConn}
			go func() {
				defer serverConn.Close()
				greeting := "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready\r\n"
				if tc.preauth {
					greeting = "* PREAUTH ready\r\n"
				}
				io.WriteString(serverConn, greeting)
				br := bufio.NewReader(serverConn)
				var idleTag string
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					name := fields[0]
					if len(fields) > 1 {
						name = fields[1]
					}
					if idleTag != "" && name != "DONE" {
						name = "VIOLATION"
					}
					srv.mutex.Lock()
					srv.lines = append(srv.lines, name)
					srv.mutex.Unlock()
					switch name {
					case "LOGIN":
						if tc.untaggedLogin {
							io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 IDLE MOVE\r\n")
						}
						io.WriteString(serverConn, fields[0]+" OK logged in\r\n")
					case "CAPABILITY":
						capsRequested <- struct{}{}
						<-releaseCaps
						io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 IDLE MOVE\r\n"+fields[0]+" OK caps\r\n")
					case "IDLE":
						idleTag = fields[0]
						io.WriteString(serverConn, "+ idling\r\n")
					case "DONE":
						io.WriteString(serverConn, idleTag+" OK idle completed\r\n")
						idleTag = ""
					default:
						io.WriteString(serverConn, fields[0]+" OK done\r\n")
					}
				}
			}()
			client := imapclient.New(clientConn, &imapclient.Options{AutoIdle: 5 * time.Millisecond, ResponseTimeout: time.Second})
			t.Cleanup(func() { client.Close() })
			if tc.preauth {
				if err := client.WaitGreeting(); err != nil {
					t.Fatal(err)
				}
			} else if err := client.Login("user", "pass").Wait(); err != nil {
				t.Fatal(err)
			}
			if !tc.untaggedLogin {
				select {
				case <-capsRequested:
				case <-time.After(time.Second):
					t.Fatal("capability refresh never started")
				}
				// A timer may expire while capabilities are unavailable. The
				// CAPABILITY completion must then arrange another chance to idle.
				time.Sleep(20 * time.Millisecond)
				if got := srv.received(); strings.Contains(got, "IDLE") {
					t.Fatalf("IDLE started before capability refresh completed: %s", got)
				}
			}
			close(releaseCaps)
			if got, ok := srv.waitFor(tc.wantFirstIdle); !ok {
				t.Fatalf("received %q, want %q", got, tc.wantFirstIdle)
			}
			if caps := client.Caps(); !caps.Has(imap.CapMove) || caps.Has(imap.AuthCap("PLAIN")) {
				t.Errorf("capabilities after transition = %v", caps)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("NOOP after automatic IDLE: %v", err)
			}
			want := tc.wantFirstIdle + " DONE NOOP IDLE"
			if got, ok := srv.waitFor(want); !ok {
				t.Fatalf("received %q, want %q", got, want)
			}
		})
	}
}
