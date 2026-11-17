package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestRespTextCodeUnclosedBracketDoesNotCrossLineBoundary(t *testing.T) {
	t.Run("unclosed_metadata_does_not_splice_next_response", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(3 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 METADATA] ready\r\n")
			_ = bw.Flush()

			line1, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line2, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag1, _, _ := strings.Cut(line1, " ")
			tag2, _, _ := strings.Cut(line2, " ")
			// tag1's response code is missing ']' before CRLF; DiscardUntilByte(']')
			// must not cross CRLF into tag2's [OVERQUOTA] bracket.
			_, _ = fmt.Fprintf(bw, "%s OK [METADATA LONGENTRIES 256\r\n%s NO [OVERQUOTA] quota exceeded\r\n", tag1, tag2)
			_ = bw.Flush()
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 1 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}

		metaCmd := c.GetMetadata("INBOX", []string{"/private/comment"}, nil)
		noopCmd := c.Noop()

		if _, err := metaCmd.Wait(); err == nil {
			t.Fatalf("GetMetadata.Wait() = nil on unclosed [METADATA line; spliced across CRLF into next tagged response")
		}
		if err := noopCmd.Wait(); err == nil {
			t.Fatalf("Noop.Wait() = nil after broken connection framing, want error")
		}
	})

	t.Run("unclosed_unknown_code_does_not_splice_next_response", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(3 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
			_ = bw.Flush()

			line1, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line2, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag1, _, _ := strings.Cut(line1, " ")
			tag2, _, _ := strings.Cut(line2, " ")
			_, _ = fmt.Fprintf(bw, "%s OK [X-BROKEN arg\r\n%s NO [ALERT] boom\r\n", tag1, tag2)
			_ = bw.Flush()
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 1 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}

		cmd1 := c.Noop()
		cmd2 := c.Noop()
		if err := cmd1.Wait(); err == nil {
			t.Fatalf("first Noop.Wait() = nil on unclosed [X-BROKEN line; spliced across CRLF")
		}
		if err := cmd2.Wait(); err == nil {
			t.Fatalf("second Noop.Wait() = nil after broken framing, want error")
		}
	})

	t.Run("unclosed_untagged_malformed_code_does_not_swallow_tagged_no", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(3 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(3 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
			_ = bw.Flush()

			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			_, _ = fmt.Fprintf(bw, "* OK [UIDVALIDITY 1713632002544\r\n* BAD [ALERT] swallowed\r\n%s OK completed\r\n", tag)
			_ = bw.Flush()
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 1 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}

		if err := c.Noop().Wait(); err == nil {
			t.Fatalf("Noop.Wait() = nil; unclosed untagged [UIDVALIDITY crossed CRLF and swallowed next line")
		}
	})
}

func TestMalformedClosedRespTextCodeDoesNotDeselectMailbox(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		bw := bufio.NewWriter(sConn)
		br := bufio.NewReader(sConn)
		_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 QRESYNC] ready\r\n")
		_ = bw.Flush()

		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		_, _ = fmt.Fprintf(bw, "* 3 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n%s OK [READ-WRITE] selected\r\n", tag)
		_ = bw.Flush()

		line, err = br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ = strings.Cut(line, " ")
		_, _ = fmt.Fprintf(bw, "* OK [CLOSED junk] not a real CLOSED code\r\n* OK [closed extra] also malformed\r\n%s OK noop done\r\n", tag)
		_ = bw.Flush()
	}()

	c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("Noop: %v", err)
	}
	if got := c.State(); got != imap.ConnStateSelected {
		t.Errorf("State() = %v after malformed [CLOSED junk], want %v", got, imap.ConnStateSelected)
	}
	if mbox := c.Mailbox(); mbox == nil || mbox.Name != "INBOX" {
		t.Errorf("Mailbox() = %+v after malformed [CLOSED junk], want INBOX", mbox)
	}
}

func TestMalformedUntaggedRespTextCodesPreserveStateAndFraming(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		bw := bufio.NewWriter(sConn)
		br := bufio.NewReader(sConn)
		_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE NOTIFY] ready\r\n")
		_ = bw.Flush()

		// SELECT INBOX with valid baseline followed by malformed advisory variants.
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		_, _ = fmt.Fprintf(bw,
			"* 2 EXISTS\r\n"+
				"* FLAGS (\\Seen)\r\n"+
				"* OK [PERMANENTFLAGS (\\Seen [Imap]/Todo \\*)] valid\r\n"+
				"* OK [UIDVALIDITY 10] valid\r\n"+
				"* OK [UIDNEXT 20] valid\r\n"+
				"* OK [HIGHESTMODSEQ 30] valid\r\n"+
				"* OK [UIDVALIDITY 99 junk] bad\r\n"+
				"* OK [uidvalidity 88 junk] bad\r\n"+
				"* OK [UIDNEXT 77 junk] bad\r\n"+
				"* OK [uidnext 66 junk] bad\r\n"+
				"* OK [HIGHESTMODSEQ 55 junk] bad\r\n"+
				"* OK [highestmodseq 44 junk] bad\r\n"+
				"* OK [NOMODSEQ junk] bad\r\n"+
				"* OK [nomodseq junk] bad\r\n"+
				"* OK [PERMANENTFLAGS (\\Deleted) junk] bad\r\n"+
				"* OK [permanentflags (\\Deleted) junk] bad\r\n"+
				"* OK [permanentflags] bad\r\n"+
				"* OK [permanentflags (\"quoted\")] bad\r\n"+
				"* OK [READ-ONLY junk] bad\r\n"+
				"* OK [read-only junk] bad\r\n"+
				"* OK [NOTIFICATIONOVERFLOW junk] bad\r\n"+
				"* OK [notificationoverflow junk] bad\r\n"+
				"%s OK [READ-WRITE] selected\r\n",
			tag,
		)
		_ = bw.Flush()

		// Subsequent NOOP with malformed [READ-ONLY junk] while selected.
		line, err = br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ = strings.Cut(line, " ")
		_, _ = fmt.Fprintf(bw, "* OK [read-only junk] bad\r\n%s OK [READ-ONLY junk] noop done\r\n", tag)
		_ = bw.Flush()
	}()

	var overflowCalls atomic.Int32
	c := imapclient.New(cConn, &imapclient.Options{
		ResponseTimeout: 4 * time.Second,
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			NotificationOverflow: func() {
				overflowCalls.Add(1)
			},
		},
	})
	defer c.Close()

	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	data, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if data.UIDValidity != 10 || data.UIDNext != 20 || data.HighestModSeq != 30 {
		t.Errorf("SelectData = %+v, want UIDValidity=10 UIDNext=20 HighestModSeq=30", data)
	}
	if data.ReadOnly {
		t.Errorf("SelectData.ReadOnly = true, want false")
	}
	if len(data.PermanentFlags) != 3 {
		t.Errorf("SelectData.PermanentFlags = %v, want 3 valid flags", data.PermanentFlags)
	}
	if got := overflowCalls.Load(); got != 0 {
		t.Errorf("NotificationOverflow called %d times on malformed code, want 0", got)
	}

	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("Noop after malformed codes: %v", err)
	}
	if mbox := c.Mailbox(); mbox == nil || mbox.ReadOnly {
		t.Errorf("Mailbox() = %+v, want ReadOnly=false", mbox)
	}
}

func TestMalformedTaggedRespTextCodesPreserveStateAndFraming(t *testing.T) {
	t.Run("appenduid_malformed_variants", func(t *testing.T) {
		const msg = "From: <a@example.org>\r\n\r\nbody\r\n"
		for _, code := range []string{
			"APPENDUID 3857529045 5:7",
			"appenduid 3857529045 5 junk",
			"APPENDUID 0 5",
			"APPENDUID 3857529045 0",
		} {
			t.Run(code, func(t *testing.T) {
				client := appendUIDServer(t, "OK ["+code+"] APPEND completed")
				appendCmd := client.Append("INBOX", int64(len(msg)), nil)
				_, _ = appendCmd.Write([]byte(msg))
				_ = appendCmd.Close()
				data, err := appendCmd.Wait()
				if err != nil {
					t.Fatalf("Append().Wait() = %v, want nil", err)
				}
				if data.UIDValidity != 0 || data.UID != 0 {
					t.Errorf("AppendData = %+v, want zeroed data for malformed [%s]", data, code)
				}
				if err := client.Noop().Wait(); err != nil {
					t.Fatalf("Noop().Wait() = %v, want connection to survive", err)
				}
			})
		}
	})

	t.Run("modified_malformed_variants", func(t *testing.T) {
		for _, tc := range []struct {
			tagged string
			wantNo bool
		}{
			{tagged: "OK [MODIFIED 1,2 junk] Conditional STORE completed"},
			{tagged: "OK [modified 1,2 junk] Conditional STORE completed"},
			{tagged: "OK [modified] Conditional STORE completed"},
			{tagged: "OK [MODIFIED bad] Conditional STORE completed"},
			{tagged: "NO [modified bad] conflict", wantNo: true},
		} {
			t.Run(tc.tagged, func(t *testing.T) {
				cConn, sConn := net.Pipe()
				defer cConn.Close()
				defer sConn.Close()
				_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
				_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

				go func() {
					br := bufio.NewReader(sConn)
					_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE] ready\r\n")
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					tag, _, _ := strings.Cut(line, " ")
					_, _ = fmt.Fprintf(sConn, "* 2 EXISTS\r\n%s OK [READ-WRITE] selected\r\n", tag)

					line, err = br.ReadString('\n')
					if err != nil {
						return
					}
					tag, _, _ = strings.Cut(line, " ")
					_, _ = fmt.Fprintf(sConn, "%s %s\r\n", tag, tc.tagged)

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
					t.Fatalf("WaitGreeting: %v", err)
				}
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					t.Fatalf("Select: %v", err)
				}

				storeCmd := c.Store(imap.UIDSetNum(1, 2), &imap.StoreFlags{
					Op:     imap.StoreFlagsAdd,
					Silent: true,
					Flags:  []imap.Flag{imap.FlagSeen},
				}, &imap.StoreOptions{UnchangedSince: 10})
				_, err := storeCmd.Collect()
				if tc.wantNo {
					var imapErr *imap.Error
					if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo {
						t.Fatalf("Store.Collect() = %v, want tagged NO", err)
					}
				} else if err != nil {
					t.Fatalf("Store.Collect() = %v, want nil", err)
				}
				if got := storeCmd.Modified(); got != nil {
					t.Errorf("Modified() = %v, want nil on malformed MODIFIED code", got)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatalf("Noop after malformed MODIFIED: %v", err)
				}
			})
		}
	})
}
