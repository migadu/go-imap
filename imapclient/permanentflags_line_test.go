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

func TestPermanentFlagsPhysicalLineFraming(t *testing.T) {
	longKeyword := "[Imap]/" + strings.Repeat("a", 5000) + "]tail"
	bracketKeyword := "[Imap]/" + strings.Repeat("]", 16<<10)
	for _, tc := range []struct {
		name      string
		line      string
		flags     []imap.Flag
		wantError bool
	}{
		{"long_text_after_unclosed_list", "(Todo] " + strings.Repeat("a", 5000), []imap.Flag{imap.FlagSeen}, false},
		// Todo] and completed can be keywords in a completed list. With no
		// outer ']', this ambiguous input must retain the framing error.
		{"completed_list_missing_bracket", "(Todo] completed)", nil, true},
		{"long_bracket_keyword", "(" + longKeyword + ")] text", []imap.Flag{imap.Flag(longKeyword)}, false},
		{"many_keyword_brackets", "(" + bracketKeyword + ")] text", []imap.Flag{imap.Flag(bracketKeyword)}, false},
		{"empty_list", "()] text", []imap.Flag{}, false},
		{"no_response_text", "(\\Deleted)]", []imap.Flag{imap.FlagDeleted}, false},
		{"empty_response_text", "(\\Deleted)] ", []imap.Flag{imap.FlagDeleted}, false},
		{"malformed_list_trailing_junk", "(\\Deleted) junk] text", []imap.Flag{imap.FlagSeen}, false},
		{"bracket_keyword_then_junk", "([Imap]/Todo) junk] text", []imap.Flag{imap.FlagSeen}, false},
		{"missing_response_text_separator", "(\\Deleted)]junk", nil, true},
		{"unclosed_code", "(Todo", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			go func() {
				br := bufio.NewReader(serverConn)
				_, _ = fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				_, _ = fmt.Fprintf(serverConn, "* 1 EXISTS\r\n* OK [PERMANENTFLAGS (\\Seen)] valid\r\n* OK [PERMANENTFLAGS %s\r\n%s OK [READ-WRITE] selected\r\n", tc.line, tag)
				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ = strings.Cut(line, " ")
				_, _ = fmt.Fprintf(serverConn, "%s OK still framed\r\n", tag)
			}()
			c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
			defer c.Close()
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			data, err := c.Select("INBOX", nil).Wait()
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid physical-line framing was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("SELECT: %v", err)
			}
			if !slices.Equal(data.PermanentFlags, tc.flags) || data.PermanentFlags == nil {
				t.Errorf("PermanentFlags = %q, want %q", data.PermanentFlags, tc.flags)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatalf("NOOP after PERMANENTFLAGS: %v", err)
			}
		})
	}
}

func TestPermanentFlagsMalformedGreetingPreservesErrorText(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	text := strings.Repeat("a", 5000) + " completed"
	go func() {
		_, _ = fmt.Fprintf(serverConn, "* BYE [PERMANENTFLAGS (Todo] %s\r\n", text)
	}()
	c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	var imapErr *imap.Error
	if err := c.WaitGreeting(); !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBye || imapErr.Code != "" || imapErr.Text != text {
		t.Fatalf("greeting error = %v, want original BYE text and an ignored code", err)
	}
}

func TestPermanentFlagsResponseBudgetPreventsPublication(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		br := bufio.NewReader(serverConn)
		_, _ = fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		_, _ = fmt.Fprintf(serverConn, "* 1 EXISTS\r\n* OK [PERMANENTFLAGS (\\Seen)] valid\r\n* OK [PERMANENTFLAGS (\\Deleted)] %s\r\n%s OK selected\r\n", strings.Repeat("a", 8<<20), tag)
	}()
	c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	data, err := c.Select("INBOX", nil).Wait()
	if err == nil || !strings.Contains(err.Error(), "max size exceeded") {
		t.Fatalf("oversized response error = %v, want syntax budget error", err)
	}
	if !slices.Equal(data.PermanentFlags, []imap.Flag{imap.FlagSeen}) {
		t.Errorf("oversized response published flags: %q", data.PermanentFlags)
	}
}
