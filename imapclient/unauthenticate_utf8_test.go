package imapclient

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
)

// UNAUTHENTICATE ends what ENABLE turned on (RFC 8437 §3). UTF8=ACCEPT let the
// server send raw UTF-8 in quoted strings (RFC 6855 §3); once it is gone, so
// is that. The decoder used to keep allowing it until it next learnt the
// capabilities, and for good if UNAUTHENTICATE's OK carried them. Issue #166.
func TestUnauthenticateEndsQuotedUTF8(t *testing.T) {
	for _, code := range []string{"", "[CAPABILITY IMAP4rev1 UNAUTHENTICATE] "} {
		t.Run(map[bool]string{false: "capabilities asked for", true: "capabilities in the OK"}[code != ""], func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			// The answer to the CAPABILITY sent after UNAUTHENTICATE waits
			// until the test has looked.
			looked := make(chan struct{})
			go func() {
				io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 UNAUTHENTICATE] ready\r\n")
				br := bufio.NewReader(serverConn)
				unauthenticated := false
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					tag, cmd, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
					switch {
					case strings.HasPrefix(cmd, "ENABLE"):
						io.WriteString(serverConn, "* ENABLED UTF8=ACCEPT\r\n"+tag+" OK done\r\n")
					case cmd == "UNAUTHENTICATE":
						unauthenticated = true
						io.WriteString(serverConn, tag+" OK "+code+"done\r\n")
					case cmd == "CAPABILITY":
						if unauthenticated {
							<-looked
						}
						io.WriteString(serverConn, "* CAPABILITY IMAP4rev1 UNAUTHENTICATE\r\n"+tag+" OK done\r\n")
					default:
						io.WriteString(serverConn, tag+" OK done\r\n")
					}
				}
			}()

			c := New(clientConn, nil)
			defer c.Close()
			quotedUTF8 := func() bool {
				c.mutex.Lock()
				defer c.mutex.Unlock()
				return c.dec.QuotedUTF8
			}
			if _, err := c.Enable(imap.CapUTF8Accept).Wait(); err != nil {
				t.Fatalf("Enable() = %v", err)
			}
			if !quotedUTF8() {
				t.Fatalf("UTF8=ACCEPT enabled, but the decoder does not allow UTF-8 in quoted strings")
			}
			if err := c.Unauthenticate().Wait(); err != nil {
				t.Fatalf("Unauthenticate() = %v", err)
			}
			if quotedUTF8() {
				t.Errorf("after UNAUTHENTICATE the decoder still allows UTF-8 in quoted strings")
			}
			close(looked)
			c.Caps()
			if quotedUTF8() {
				t.Errorf("once the capabilities are known again, the decoder still allows UTF-8 in quoted strings")
			}
		})
	}
}
