package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestPermanentFlagsStructuralRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, list string
		valid      bool
		fatal      bool
	}{
		{name: "unmatched_right_bracket", list: `(Todo] *)]`},
		{name: "leading_right_bracket", list: `(]lead *)]`},
		{name: "unmatched_left_bracket", list: `([open *)]`},
		{name: "mixed_brackets", list: `(a[b]c] *)]`},
		{name: "many_brackets", list: "(" + strings.Repeat("]", 16<<10) + " *)]"},
		{name: "nested_invalid_group", list: `([Imap]/Todo (bad))]`},
		{name: "nested_quoted_delimiters", list: `([Imap]/Todo ("a)b]c"))]`},
		{name: "quoted_escaped_quote", list: `([Imap]/Todo "a\" ) ] z")]`},
		{name: "quoted_escaped_backslash", list: `([Imap]/Todo "a\\")]`},
		{name: "unclosed_list_recovers_at_bracket", list: `(Todo]`},
		{name: "valid_arbitrary_bracket_keywords", list: `(Todo] a[b]c] [open)]`, valid: true},
		{name: "closed_valid_list_missing_code_bracket", list: `(Todo] completed)`, fatal: true},
		{name: "closed_malformed_list_missing_code_bracket", list: `(Todo] *)`, fatal: true},
		{name: "closed_nested_list_missing_code_bracket", list: `(Todo] (bad))`, fatal: true},
		{name: "unclosed_quoted_string", list: `([Imap]/Todo "unclosed)]`, fatal: true},
	} {
		for _, bye := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bye=%v", tc.name, bye), func(t *testing.T) {
				clientConn, serverConn := net.Pipe()
				defer clientConn.Close()
				defer serverConn.Close()
				_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
				_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
				const caption = "Server shutting down"
				go func() {
					if bye {
						_, _ = fmt.Fprintf(serverConn, "* BYE [PERMANENTFLAGS %s %s\r\n", tc.list, caption)
						return
					}
					br := bufio.NewReader(serverConn)
					_, _ = fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					tag, _, _ := strings.Cut(line, " ")
					_, _ = fmt.Fprintf(serverConn, "* 1 EXISTS\r\n* OK [PERMANENTFLAGS (\\Seen)] initial\r\n* OK [PERMANENTFLAGS %s %s\r\n%s OK [READ-WRITE] selected\r\n", tc.list, caption, tag)
					line, err = br.ReadString('\n')
					if err != nil {
						return
					}
					tag, _, _ = strings.Cut(line, " ")
					_, _ = fmt.Fprintf(serverConn, "%s OK next command\r\n", tag)
				}()

				c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
				defer c.Close()
				if bye {
					err := c.WaitGreeting()
					var imapErr *imap.Error
					if tc.fatal {
						if err == nil || errors.As(err, &imapErr) {
							t.Fatalf("invalid framing reported a BYE result: %v", err)
						}
						return
					}
					wantCode := imap.ResponseCode("")
					if tc.valid {
						wantCode = "PERMANENTFLAGS"
					}
					if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBye || imapErr.Code != wantCode || imapErr.Text != caption {
						t.Fatalf("greeting = %v, want original BYE text and code %q", err, wantCode)
					}
					return
				}
				if err := c.WaitGreeting(); err != nil {
					t.Fatal(err)
				}
				data, err := c.Select("INBOX", nil).Wait()
				if tc.fatal {
					if err == nil {
						t.Fatal("invalid framing completed SELECT")
					}
				} else if err != nil {
					t.Fatalf("SELECT: %v", err)
				}
				want := []imap.Flag{imap.FlagSeen}
				if tc.valid {
					want = []imap.Flag{"Todo]", "a[b]c]", "[open"}
				}
				if !slices.Equal(data.PermanentFlags, want) {
					t.Errorf("PermanentFlags = %v, want %v", data.PermanentFlags, want)
				}
				if !tc.fatal {
					if err := c.Noop().Wait(); err != nil {
						t.Fatalf("NOOP after SELECT: %v", err)
					}
				}
			})
		}
	}
}
