package imapclient_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func TestResponseCodesCaseInsensitivityAndMetadataSubcodeFraming(t *testing.T) {
	t.Run("closed_switch_mailbox_isolation", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE QRESYNC] ready\r\n")
			_ = bw.Flush()

			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString("* 5 EXISTS\r\n* OK [UIDVALIDITY 1] ok\r\n" + tag + " OK [READ-WRITE] selected\r\n")
			_ = bw.Flush()

			line, _ = br.ReadString('\n')
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString("* OK [closed] Previous mailbox is now closed\r\n")
			_, _ = bw.WriteString("* 10 EXISTS\r\n")
			_, _ = bw.WriteString("* FLAGS (\\Seen)\r\n")
			_, _ = bw.WriteString("* OK [permanentflags (\\Seen \\*)] ok\r\n")
			_, _ = bw.WriteString("* OK [uidvalidity 42] ok\r\n")
			_, _ = bw.WriteString("* OK [uidnext 11] ok\r\n")
			_, _ = bw.WriteString("* OK [highestmodseq 99] ok\r\n")
			_, _ = bw.WriteString(tag + " OK [read-only] EXAMINE completed\r\n")
			_ = bw.Flush()
		}()

		var inboxSpuriousExists uint32
		var c *imapclient.Client
		c = imapclient.New(cConn, &imapclient.Options{
			ResponseTimeout: 4 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				Mailbox: func(data *imapclient.UnilateralDataMailbox) {
					if mbox := c.Mailbox(); mbox != nil && mbox.Name == "INBOX" && data.NumMessages != nil {
						atomic.StoreUint32(&inboxSpuriousExists, *data.NumMessages)
					}
				},
			},
		})
		defer c.Close()

		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatalf("Select INBOX: %v", err)
		}

		selData, err := c.Select("Archive", &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			t.Fatalf("Examine Archive: %v", err)
		}
		if got := atomic.LoadUint32(&inboxSpuriousExists); got != 0 {
			t.Errorf("Archive's EXISTS=%d leaked onto INBOX because [closed] was not recognized", got)
		}
		if selData.NumMessages != 10 || selData.UIDValidity != 42 || selData.UIDNext != 11 || selData.HighestModSeq != 99 || !selData.ReadOnly || len(selData.PermanentFlags) != 2 {
			t.Errorf("SelectData = %+v, want NumMessages=10 UIDValidity=42 UIDNext=11 HighestModSeq=99 ReadOnly=true PermanentFlags=2", selData)
		}
	})

	t.Run("tagged_metadata_lowercase_code", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 METADATA] ready\r\n")
			_ = bw.Flush()

			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString(tag + " OK [metadata longentries 256] GETMETADATA completed\r\n")
			_ = bw.Flush()
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}

		maxSize := uint32(10)
		data, err := c.GetMetadata("INBOX", []string{"/private/comment"}, &imap.GetMetadataOptions{MaxSize: &maxSize}).Wait()
		if err != nil {
			t.Fatalf("GetMetadata: %v", err)
		}
		if data.ResponseCodeData == nil || data.ResponseCodeData.Size != 256 {
			t.Errorf("ResponseCodeData = %+v, want Size=256", data.ResponseCodeData)
		}
	})

	t.Run("tagged_metadata_unicode_subcode_does_not_kill_conn", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 METADATA] ready\r\n")
			_ = bw.Flush()

			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString(tag + " OK [METADATA MAX\u017fIZE 256] GETMETADATA completed\r\n")
			_ = bw.Flush()

			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString(tag + " OK NOOP completed\r\n")
			_ = bw.Flush()
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}

		data, err := c.GetMetadata("INBOX", []string{"/private/comment"}, nil).Wait()
		if err != nil {
			t.Fatalf("GetMetadata killed connection on unrecognized METADATA subcode: %v", err)
		}
		if data.ResponseCodeData != nil {
			t.Errorf("Unicode lookalike MAXſIZE populated ResponseCodeData = %+v", data.ResponseCodeData)
		}
		if err := c.Noop().Wait(); err != nil {
			t.Fatalf("subsequent Noop failed: %v", err)
		}
	})
}

func TestESearchLowercaseUIDToken(t *testing.T) {
	t.Run("uid_all_does_not_kill_connection", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2] ready\r\n")
			_ = bw.Flush()

			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString("* 3 EXISTS\r\n" + tag + " OK [READ-WRITE] selected\r\n")
			_ = bw.Flush()

			line, _ = br.ReadString('\n')
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString(fmt.Sprintf("* ESEARCH (TAG %q) uid ALL 10:12\r\n%s OK UID SEARCH completed\r\n", tag, tag))
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
		data, err := c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true}).Wait()
		if err != nil {
			t.Fatalf("UIDSearch failed on lowercase 'uid' in ESEARCH: %v", err)
		}
		uidSet, ok := data.All.(imap.UIDSet)
		if !ok || uidSet.String() != "10:12" {
			t.Fatalf("data.All = %T(%v), want UIDSet(10:12)", data.All, data.All)
		}
	})

	t.Run("uid_all_modseq_preserves_results", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE] ready\r\n")
			_ = bw.Flush()

			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString("* 3 EXISTS\r\n" + tag + " OK [READ-WRITE] selected\r\n")
			_ = bw.Flush()

			line, _ = br.ReadString('\n')
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString(fmt.Sprintf("* ESEARCH (TAG %q) uid ALL 10:12 (MODSEQ 99)\r\n%s OK UID SEARCH completed\r\n", tag, tag))
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
		data, err := c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true}).Wait()
		if err != nil {
			t.Fatalf("UIDSearch: %v", err)
		}
		uidSet, ok := data.All.(imap.UIDSet)
		if !ok || uidSet.String() != "10:12" || data.ModSeq != 99 {
			t.Fatalf("silent ESEARCH data loss on lowercase 'uid': All=%T(%v) ModSeq=%d, want UIDSet(10:12) ModSeq=99", data.All, data.All, data.ModSeq)
		}
	})
}

func TestQuotaInboxCanonicalizationAndSystemFlagSearch(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		bw := bufio.NewWriter(sConn)
		br := bufio.NewReader(sConn)
		_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 QUOTA QUOTA=RES-STORAGE] ready\r\n")
		_ = bw.Flush()

		line, _ := br.ReadString('\n')
		tag, _, _ := strings.Cut(line, " ")
		_, _ = bw.WriteString("* QUOTA \"\" (storage 10 512 message 5 100)\r\n" + tag + " OK GETQUOTA completed\r\n")
		_ = bw.Flush()

		line, _ = br.ReadString('\n')
		tag, _, _ = strings.Cut(line, " ")
		_, _ = bw.WriteString("* QUOTAROOT INBOX \"\"\r\n* QUOTA \"\" (STORAGE 10 512)\r\n" + tag + " OK GETQUOTAROOT completed\r\n")
		_ = bw.Flush()
	}()

	c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
	defer c.Close()
	if err := c.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting: %v", err)
	}
	q, err := c.GetQuota("").Wait()
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if _, ok := q.Resources[imap.QuotaResourceStorage]; !ok {
		t.Errorf("Resources[QuotaResourceStorage] missing, got map %+v", q.Resources)
	}
	if _, ok := q.Resources[imap.QuotaResourceMessage]; !ok {
		t.Errorf("Resources[QuotaResourceMessage] missing, got map %+v", q.Resources)
	}
	qrs, err := c.GetQuotaRoot("inbox").Wait()
	if err != nil {
		t.Fatalf("GetQuotaRoot(inbox): %v", err)
	}
	if len(qrs) != 1 {
		t.Errorf("GetQuotaRoot(\"inbox\") dropped server QUOTAROOT INBOX response: got %+v", qrs)
	}

	memUser := imapmemserver.NewUser("user", "pass")
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create INBOX: %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMetadata: {}},
		InsecureAuth: true,
	})
	defer srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()

	var wireBuf strings.Builder
	mc, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{DebugWriter: &wireBuf})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer mc.Close()
	if err := mc.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}

	st, err := mc.Status("inbox", &imap.StatusOptions{UIDValidity: true, NumMessages: true}).Wait()
	if err != nil {
		t.Fatalf("Status(inbox): %v", err)
	}
	if st.UIDValidity == 0 || st.NumMessages == nil {
		t.Errorf("Status(\"inbox\") dropped server STATUS INBOX response: %+v", st)
	}

	val := []byte("hello")
	if err := mc.SetMetadata("inbox", map[string]*[]byte{"/private/comment": &val}).Wait(); err != nil {
		t.Fatalf("SetMetadata(inbox): %v", err)
	}
	md, err := mc.GetMetadata("inbox", []string{"/private/comment"}, nil).Wait()
	if err != nil {
		t.Fatalf("GetMetadata(inbox): %v", err)
	}
	if len(md.Entries) != 1 {
		t.Errorf("GetMetadata(\"inbox\") dropped server METADATA INBOX response: %+v", md)
	}

	if _, err := mc.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, err := mc.Search(&imap.SearchCriteria{Flag: []imap.Flag{"\\seen"}}, nil).Wait(); err != nil {
		t.Fatalf("Search(\\seen): %v", err)
	}
	if strings.Contains(wireBuf.String(), "KEYWORD \\seen") {
		t.Errorf("Search(\\seen) emitted invalid RFC 3501/9051 syntax 'KEYWORD \\seen': %s", wireBuf.String())
	}
}
