package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestPermanentFlagsBracketKeywordInMalformedClosedList(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"bare_wildcard_after_bracket_keyword", `[PERMANENTFLAGS (\Seen [Imap]/Todo *)] Flags permitted`},
		{"bare_wildcard_before_bracket_keyword", `[PERMANENTFLAGS (* [Imap]/Todo)] Flags permitted`},
		{"quoted_flag_after_bracket_keyword", `[PERMANENTFLAGS ([Imap]/Todo "Custom")] Flags permitted`},
		{"backslash_bracket_flag", `[PERMANENTFLAGS (\[Imap]/Todo \*)] Flags permitted`},
		{"unmatched_bracket_keyword_then_bare_wildcard", `[PERMANENTFLAGS (Todo]/Sub *)] Flags permitted`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cConn, sConn := net.Pipe()
			defer cConn.Close()
			defer sConn.Close()
			_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

			go func() {
				br := bufio.NewReader(sConn)
				_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				_, _ = fmt.Fprintf(sConn,
					"* 1 EXISTS\r\n"+
						"* OK [PERMANENTFLAGS (\\Seen)] initial\r\n"+
						"* OK %s\r\n"+
						"%s OK [READ-WRITE] selected\r\n",
					tc.code, tag,
				)
				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ = strings.Cut(line, " ")
				_, _ = fmt.Fprintf(sConn, "%s OK noop done\r\n", tag)
			}()

			c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
			defer c.Close()
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			data, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				t.Fatalf("Select failed on advisory malformed PERMANENTFLAGS %s: %v", tc.code, err)
			}
			if !slices.Equal(data.PermanentFlags, []imap.Flag{imap.FlagSeen}) {
				t.Errorf("PermanentFlags = %q, want [\\Seen]", data.PermanentFlags)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatalf("Noop after Select failed: %v", err)
			}
		})
	}
}

func TestPermanentFlagsByeTextNotCorruptedByBracketKeyword(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		_, _ = io.WriteString(sConn, "* BYE [PERMANENTFLAGS ([Imap] *)] Server shutting down\r\n")
	}()

	c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	err := c.WaitGreeting()
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeBye || imapErr.Code != "" || imapErr.Text != "Server shutting down" {
		t.Fatalf("greeting err = %#v, want BYE with Code=\"\" and Text=\"Server shutting down\"", err)
	}
}

func TestCopyUIDRejectsNonASCIIWhitespaceAndNEL(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp string
	}{
		{"nel_c1_ctl", "[COPYUID 7\xc2\x851\xc2\x85101]"},
		{"nbsp_utf8", "[COPYUID 7\xc2\xa01\xc2\xa0101]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cConn, sConn := net.Pipe()
			defer cConn.Close()
			defer sConn.Close()
			_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

			go func() {
				br := bufio.NewReader(sConn)
				_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev2 UIDPLUS] ready\r\n")
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				_, _ = fmt.Fprintf(sConn, "%s OK %s copied\r\n", tag, tc.resp)
			}()

			c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
			defer c.Close()
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			data, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
			if err != nil || data.UIDValidity != 0 || len(data.UIDMapping) != 0 {
				t.Fatalf("malformed %q = (%+v, %v), want zero CopyData and nil err", tc.resp, data, err)
			}
		})
	}
}
