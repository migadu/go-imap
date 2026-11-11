package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 9051 §7.2.1: "The ENABLED response occurs as a result of an ENABLE
// command." An ENABLED no ENABLE asked for, or one before authentication
// (§11.3: "clients SHOULD ignore any responses other than CAPABILITY and
// server status responses"), enables nothing. Here UTF8=ACCEPT would make
// the client send a non-ASCII SEARCH as quoted UTF-8 without CHARSET to a
// server that only speaks IMAP4rev1. Issue #167.
func TestEnabledOnlyAnswersEnable(t *testing.T) {
	const enabled = "* ENABLED UTF8=ACCEPT\r\n"
	for _, tc := range []struct {
		name     string
		greeting string
		login    bool
		enable   bool
		noop     string // sent with the answer to NOOP
		want     string // the start of the SEARCH command, after its tag
	}{
		{
			name:     "before authentication",
			greeting: "* OK [CAPABILITY IMAP4rev1 LITERAL+] ready\r\n" + enabled,
			login:    true,
			want:     "UID SEARCH CHARSET UTF-8 BODY {5+}",
		},
		{
			name:     "with no ENABLE pending",
			greeting: "* PREAUTH [CAPABILITY IMAP4rev1 LITERAL+] ready\r\n",
			noop:     enabled,
			want:     "UID SEARCH CHARSET UTF-8 BODY {5+}",
		},
		{
			name:     "in answer to ENABLE",
			greeting: "* PREAUTH [CAPABILITY IMAP4rev1 LITERAL+] ready\r\n",
			enable:   true,
			want:     `UID SEARCH BODY "café"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))

			search := make(chan string, 1)
			go func() {
				io.WriteString(serverConn, tc.greeting)
				br := bufio.NewReader(serverConn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSuffix(line, "\r\n")
					tag, cmd, _ := strings.Cut(line, " ")
					// Read a trailing non-synchronizing literal and the rest
					// of the command.
					if i := strings.LastIndex(cmd, "{"); i >= 0 && strings.HasSuffix(cmd, "+}") {
						n, _ := strconv.Atoi(cmd[i+1 : len(cmd)-2])
						if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
							return
						}
						if _, err := br.ReadString('\n'); err != nil {
							return
						}
					}
					var data, code string
					switch {
					case strings.HasPrefix(cmd, "CAPABILITY"):
						data = "* CAPABILITY IMAP4rev1 LITERAL+\r\n"
					case strings.HasPrefix(cmd, "LOGIN"):
						code = "[CAPABILITY IMAP4rev1 LITERAL+] "
					case strings.HasPrefix(cmd, "NOOP"):
						data = tc.noop
					case strings.HasPrefix(cmd, "ENABLE"):
						data = enabled
					case strings.HasPrefix(cmd, "SELECT"):
						data = "* 1 EXISTS\r\n"
					case strings.HasPrefix(cmd, "UID SEARCH"):
						search <- cmd
					}
					io.WriteString(serverConn, data+tag+" OK "+code+"done\r\n")
				}
			}()

			client := imapclient.New(clientConn, nil)
			defer client.Close()
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop() = %v", err)
			}
			if tc.login {
				if err := client.Login("user", "pass").Wait(); err != nil {
					t.Fatalf("Login() = %v", err)
				}
			}
			if tc.enable {
				data, err := client.Enable(imap.CapUTF8Accept).Wait()
				if err != nil {
					t.Fatalf("Enable() = %v", err)
				}
				if !data.Caps.Has(imap.CapUTF8Accept) {
					t.Errorf("Enable() = %v, want UTF8=ACCEPT enabled", data.Caps)
				}
			}
			if _, err := client.Select("INBOX", nil).Wait(); err != nil {
				t.Fatalf("Select() = %v", err)
			}
			if _, err := client.UIDSearch(&imap.SearchCriteria{Body: []string{"café"}}, nil).Wait(); err != nil {
				t.Fatalf("UIDSearch() = %v", err)
			}
			if got := <-search; !strings.HasPrefix(got, tc.want) {
				t.Errorf("SEARCH sent as %q, want %q", got, tc.want)
			}
		})
	}
}
