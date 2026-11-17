package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestModifiedSearchResMutationSafetyAndNoRace(t *testing.T) {
	makeCmd := func(t *testing.T) *imapclient.FetchCommand {
		t.Helper()
		cConn, sConn := net.Pipe()
		t.Cleanup(func() {
			_ = cConn.Close()
			_ = sConn.Close()
		})
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE SEARCHRES] ready\r\n")
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			_, _ = fmt.Fprintf(sConn, "%s OK [MODIFIED $] conditional store\r\n", tag)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		t.Cleanup(func() { _ = c.Close() })
		if err := c.WaitGreeting(); err != nil {
			t.Fatal(err)
		}
		cmd := c.Store(imap.UIDSetNum(1, 2), &imap.StoreFlags{
			Op:     imap.StoreFlagsAdd,
			Silent: true,
			Flags:  []imap.Flag{imap.FlagSeen},
		}, &imap.StoreOptions{UnchangedSince: 10})
		if _, err := cmd.Collect(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	cmd1, cmd2 := makeCmd(t), makeCmd(t)
	var wg sync.WaitGroup
	var got1, got2 string
	var isRes1, isRes2 bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		u := cmd1.Modified().(imap.UIDSet)
		u.AddNum(42)
		got1, isRes1 = u.String(), imap.IsSearchRes(u)
	}()
	go func() {
		defer wg.Done()
		u := cmd2.Modified().(imap.UIDSet)
		u.AddNum(99)
		got2, isRes2 = u.String(), imap.IsSearchRes(u)
	}()
	wg.Wait()

	if isRes1 || isRes2 || got1 != "42" || got2 != "99" {
		t.Fatalf("after AddNum on Modified(): isRes=(%v,%v) got=(%q,%q), want (false,false)/(\"42\",\"99\")", isRes1, isRes2, got1, got2)
	}
	if !imap.IsSearchRes(cmd1.Modified()) || cmd1.Modified().String() != "$" {
		t.Fatalf("cmd1.Modified() = %v (IsSearchRes=%v), want SearchRes ($)", cmd1.Modified(), imap.IsSearchRes(cmd1.Modified()))
	}
}

func TestCopyUIDMalformedAndUnclosedGuards(t *testing.T) {
	t.Run("malformed_copyuid_does_not_clobber_movedata", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev2 UIDPLUS MOVE] ready\r\n")
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
			_, _ = fmt.Fprintf(sConn,
				"* OK [COPYUID 7 1 31] moved\r\n"+
					"* OK [COPYUID 7 1 99 junk] malformed advisory\r\n"+
					"* 1 EXPUNGE\r\n"+
					"%s OK [COPYUID bad] move completed\r\n",
				tag,
			)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		data, err := c.Move(imap.UIDSetNum(1), "Archive").Wait()
		if err != nil {
			t.Fatalf("Move: %v", err)
		}
		if data.UIDValidity != 7 || len(data.UIDMapping) != 1 {
			t.Errorf("MoveData clobbered by malformed COPYUID: got %+v, want UIDValidity=7 len=1", data)
		}
	})

	t.Run("malformed_copyuid_on_no_clears_error_code", func(t *testing.T) {
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
			_, _ = fmt.Fprintf(sConn, "%s NO [COPYUID bad] copy failed\r\n", tag)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		_, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
		var imapErr *imap.Error
		if !errors.As(err, &imapErr) || imapErr.Code != "" {
			t.Errorf("Copy error = %#v, want *imap.Error with Code == \"\"", err)
		}
	})

	t.Run("unclosed_copyuid_does_not_mutate_data_before_bracket_check", func(t *testing.T) {
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
			_, _ = fmt.Fprintf(sConn, "%s OK [COPYUID 7 1 101\r\n", tag)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		data, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
		if err == nil || data.UIDValidity != 0 || len(data.UIDMapping) != 0 {
			t.Errorf("unclosed [COPYUID = (%+v, %v), want zero CopyData and framing error", data, err)
		}
	})

	t.Run("copyuid_rejects_missing_sp_and_ascii_ctl", func(t *testing.T) {
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
			_, _ = fmt.Fprintf(sConn, "%s OK [COPYUID\t7\t1\t101] copied\r\n", tag)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		data, err := c.Copy(imap.UIDSetNum(1), "Archive").Wait()
		if err != nil || data.UIDValidity != 0 || len(data.UIDMapping) != 0 {
			t.Errorf("malformed [COPYUID\\t7\\t1\\t101] = (%+v, %v), want zero CopyData and nil err", data, err)
		}
	})
}

func TestCapabilityRespCodeEmptyMalformedAndUnclosedGuards(t *testing.T) {
	t.Run("empty_capability_code_preserves_refresh_and_utf7", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		selectLineCh := make(chan string, 1)
		go func() {
			br := bufio.NewReader(sConn)
			_, _ = io.WriteString(sConn, "* OK [CAPABILITY IMAP4rev1 IDLE] ready\r\n")
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			_, _ = fmt.Fprintf(sConn, "%s OK [CAPABILITY] logged in\r\n", tag)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				tag, rest, _ := strings.Cut(line, " ")
				if strings.HasPrefix(rest, "CAPABILITY") {
					_, _ = fmt.Fprintf(sConn, "* CAPABILITY IMAP4rev1 IDLE MOVE\r\n%s OK caps\r\n", tag)
				} else if strings.HasPrefix(rest, "SELECT ") {
					selectLineCh <- line
					_, _ = fmt.Fprintf(sConn, "* 0 EXISTS\r\n%s OK [READ-WRITE] selected\r\n", tag)
					return
				}
			}
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if err := c.Login("u", "p").Wait(); err != nil {
			t.Fatal(err)
		}
		if !c.Caps().Has(imap.CapIMAP4rev1) || !c.Caps().Has(imap.CapMove) {
			t.Errorf("Caps() after empty [CAPABILITY] = %v, want refreshed IMAP4rev1+MOVE", c.Caps())
		}
		if _, err := c.Select("Caf\u00e9", nil).Wait(); err != nil {
			t.Fatal(err)
		}
		selectLine := <-selectLineCh
		if !strings.Contains(selectLine, "Caf&AOk-") {
			t.Errorf("empty [CAPABILITY] switched IMAP4rev1 client to raw UTF-8: %q", selectLine)
		}
	})

	t.Run("malformed_capability_code_ignored_without_killing_conn", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev1 IDLE] ready\r\n")
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			_, _ = fmt.Fprintf(sConn, "* OK [CAPABILITY IMAP4rev2 \"bad\"] advisory\r\n%s OK [CAPABILITY(bad)] done\r\n", tag)

			line, err = br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ = strings.Cut(line, " ")
			_, _ = fmt.Fprintf(sConn, "%s OK still alive\r\n", tag)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("first Noop: %v", err)
		}
		if !c.Caps().Has(imap.CapIMAP4rev1) || c.Caps().Has(imap.CapIMAP4rev2) {
			t.Errorf("Caps() mutated by malformed [CAPABILITY]: %v", c.Caps())
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("second Noop: %v", err)
		}
	})

	t.Run("unclosed_capability_code_does_not_mutate_caps", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			br := bufio.NewReader(sConn)
			_, _ = io.WriteString(sConn, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ := strings.Cut(line, " ")
			_, _ = fmt.Fprintf(sConn, "%s OK [CAPABILITY IMAP4rev2 MOVE\r\n", tag)
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatal(err)
		}
		if err := c.Noop().Wait(); err == nil {
			t.Fatalf("Noop succeeded on unclosed [CAPABILITY")
		}
		if c.Caps().Has(imap.CapMove) {
			t.Errorf("unclosed [CAPABILITY mutated Caps(): %v", c.Caps())
		}
	})
}

func TestPermanentFlagsUnclosedParenAndZeroUIDNumbersIgnored(t *testing.T) {
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
				"* OK [PERMANENTFLAGS (\\Seen [Imap]/Todo \\*)] valid\r\n"+
				"* OK [UIDVALIDITY 10] valid\r\n"+
				"* OK [UIDNEXT 20] valid\r\n"+
				"* OK [PERMANENTFLAGS (\\Deleted] missing close paren\r\n"+
				"* OK [PERMANENTFLAGS (\\Flagged] text with [note)]\r\n"+
				"* OK [PERMANENTFLAGS (Todo] missing close paren\r\n"+
				"* OK [PERMANENTFLAGS (Todo] bad (text)\r\n"+
				"* OK [PERMANENTFLAGS (] empty unclosed\r\n"+
				"* OK [PERMANENTFLAGS ([Imap]/Todo) junk]\r\n"+
				"* OK [UIDVALIDITY 0] invalid nz-number\r\n"+
				"* OK [UIDNEXT 0] invalid nz-number\r\n"+
				"%s OK [READ-WRITE] selected\r\n",
			tag,
		)

		line, err = br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ = strings.Cut(line, " ")
		_, _ = fmt.Fprintf(sConn, "%s OK still framed\r\n", tag)
	}()

	c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatal(err)
	}
	data, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}
	if len(data.PermanentFlags) != 3 || data.UIDValidity != 10 || data.UIDNext != 20 {
		t.Errorf("SelectData = %+v, want 3 flags, UIDValidity=10, UIDNext=20", data)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("Noop after malformed PERMANENTFLAGS/UIDVALIDITY: %v", err)
	}
}
