package imapclient_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type selectCondStoreSession struct {
	imapserver.Session
}

func (s *selectCondStoreSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data, err := s.Session.Select(ctx, mailbox, options)
	if err != nil {
		return nil, err
	}
	if mailbox == "NoMod" {
		data.HighestModSeq = 0
	} else {
		data.HighestModSeq = 4242
	}
	return data, nil
}

func (s *selectCondStoreSession) HighestModSeq() uint64 {
	return 4242
}

func TestSelectDoesNotInheritPreviousMailboxHighestModSeq(t *testing.T) {
	t.Run("pipelined_after_enabling_command", func(t *testing.T) {
		mem := imapmemserver.New()
		user := imapmemserver.NewUser(testUsername, testPassword)
		for _, box := range []string{"INBOX", "NoMod"} {
			if err := user.Create(context.Background(), box, nil); err != nil {
				t.Fatal(err)
			}
		}
		mem.AddUser(user)

		srv := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &selectCondStoreSession{Session: mem.NewSession()}, nil, nil
			},
			Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapCondStore: {}},
			InsecureAuth: true,
		})
		t.Cleanup(func() { srv.Close() })

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go srv.Serve(ln)

		c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })

		if err := c.Login(testUsername, testPassword).Wait(); err != nil {
			t.Fatal(err)
		}
		appendCmd := c.Append("INBOX", 4, nil)
		if _, err := appendCmd.Write([]byte("test")); err != nil {
			t.Fatal(err)
		}
		if err := appendCmd.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := appendCmd.Wait(); err != nil {
			t.Fatal(err)
		}

		// Select INBOX without enabling CONDSTORE.
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}

		// Pipeline FETCH 1 (MODSEQ) (first CONDSTORE-enabling command on INBOX,
		// which emits * OK [HIGHESTMODSEQ 4242] for INBOX) ahead of SELECT NoMod
		// (which emits * OK [CLOSED] and * OK [NOMODSEQ]).
		fetchCmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{ModSeq: true})
		selectCmd := c.Select("NoMod", nil)

		if _, err := fetchCmd.Collect(); err != nil {
			t.Fatalf("Fetch.Collect: %v", err)
		}
		selectData, err := selectCmd.Wait()
		if err != nil {
			t.Fatalf("Select.Wait: %v", err)
		}
		if selectData.HighestModSeq != 0 {
			t.Fatalf("Select(NoMod).HighestModSeq = %d, want 0", selectData.HighestModSeq)
		}
	})

	t.Run("pre_CLOSED_watermark_for_previous_mailbox", func(t *testing.T) {
		sConn, cConn := net.Pipe()
		t.Cleanup(func() {
			sConn.Close()
			cConn.Close()
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			br := bufio.NewReader(sConn)
			_, _ = sConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] ready\r\n"))

			// 1. SELECT INBOX
			line1, _ := br.ReadString('\n')
			tag1 := strings.SplitN(line1, " ", 2)[0]
			_, _ = sConn.Write([]byte(
				"* 1 EXISTS\r\n" +
					"* OK [UIDVALIDITY 1] ok\r\n" +
					"* OK [UIDNEXT 2] ok\r\n" +
					"* FLAGS (\\Seen)\r\n" +
					"* OK [PERMANENTFLAGS (\\Seen)]\r\n" +
					"* OK [HIGHESTMODSEQ 10] ok\r\n" +
					tag1 + " OK [READ-WRITE] SELECT completed\r\n",
			))

			// 2. SELECT NoMod: RFC 7162 §3.2.11 places all responses for the
			// closing mailbox before [CLOSED], and the new mailbox sends [NOMODSEQ].
			line2, _ := br.ReadString('\n')
			tag2 := strings.SplitN(line2, " ", 2)[0]
			_, _ = sConn.Write([]byte(
				"* OK [HIGHESTMODSEQ 3] closing watermark\r\n" +
					"* OK [CLOSED] Previous mailbox is now closed\r\n" +
					"* 0 EXISTS\r\n" +
					"* OK [UIDVALIDITY 2] ok\r\n" +
					"* OK [UIDNEXT 1] ok\r\n" +
					"* FLAGS (\\Seen)\r\n" +
					"* OK [PERMANENTFLAGS (\\Seen)]\r\n" +
					"* OK [NOMODSEQ] no modseq\r\n" +
					tag2 + " OK [READ-WRITE] SELECT completed\r\n",
			))
		}()

		c := imapclient.New(cConn, nil)
		t.Cleanup(func() { c.Close() })

		if _, err := c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait(); err != nil {
			t.Fatalf("Select(INBOX): %v", err)
		}
		selectData, err := c.Select("NoMod", nil).Wait()
		<-done
		if err != nil {
			t.Fatalf("Select(NoMod): %v", err)
		}
		if selectData.HighestModSeq != 0 {
			t.Fatalf("Select(NoMod).HighestModSeq = %d, want 0", selectData.HighestModSeq)
		}
	})
}
