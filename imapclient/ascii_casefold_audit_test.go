package imapclient_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func setupMemClientServerForCaseFold(t *testing.T, utf8Accept bool) *imapclient.Client {
	t.Helper()

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
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:  {},
			imap.CapEnable:     {},
			imap.CapUTF8Accept: {},
			imap.CapBinary:     {},
			imap.CapPreview:    {},
		},
		InsecureAuth: true,
	})
	t.Cleanup(func() { _ = srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if utf8Accept {
		if _, err := c.Enable(imap.CapUTF8Accept).Wait(); err != nil {
			t.Fatalf("Enable UTF8=ACCEPT: %v", err)
		}
	}
	return c
}

func appendAndSelectInboxForCaseFold(t *testing.T, c *imapclient.Client, rawMsg string) {
	t.Helper()
	appendCmd := c.Append("INBOX", int64(len(rawMsg)), nil)
	if _, err := appendCmd.Write([]byte(rawMsg)); err != nil {
		t.Fatalf("Append write: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("Append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("Append wait: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}
}

func TestBodyStructureUnicodeMessageLookalikeDoesNotCrashClient(t *testing.T) {
	for _, utf8Accept := range []bool{false, true} {
		name := "rev1"
		if utf8Accept {
			name = "utf8_accept"
		}
		t.Run(name, func(t *testing.T) {
			c := setupMemClientServerForCaseFold(t, utf8Accept)
			const rawMsg = "From: alice@example.com\r\nContent-Type: me\u017f\u017fage/rfc822\r\n\r\nhello\r\n"
			appendAndSelectInboxForCaseFold(t, c, rawMsg)

			var seqSet imap.SeqSet
			seqSet.AddNum(1)
			msgs, err := c.Fetch(seqSet, &imap.FetchOptions{
				BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			}).Collect()
			if err != nil {
				t.Fatalf("Fetch BODYSTRUCTURE crashed on meſſage/rfc822: %v", err)
			}
			if len(msgs) != 1 || msgs[0].BodyStructure == nil {
				t.Fatalf("unexpected Fetch result: %+v", msgs)
			}
		})
	}
}

func TestMediaTypeAndBodyFldParamASCIIFolding(t *testing.T) {
	sp := &imap.BodyStructureSinglePart{Type: "APPLICATION", Subtype: "p\u212acs7-mime"}
	if got := sp.MediaType(); got == "application/pkcs7-mime" {
		t.Errorf("BodyStructureSinglePart.MediaType() collapsed U+212A lookalike to %q", got)
	} else if got != "application/p\u212acs7-mime" {
		t.Errorf("BodyStructureSinglePart.MediaType() = %q, want %q", got, "application/p\u212acs7-mime")
	}

	mp := &imap.BodyStructureMultiPart{Subtype: "M\u0130XED"}
	if got := mp.MediaType(); got == "multipart/mixed" {
		t.Errorf("BodyStructureMultiPart.MediaType() collapsed U+0130 lookalike to %q", got)
	} else if got != "multipart/m\u0130xed" {
		t.Errorf("BodyStructureMultiPart.MediaType() = %q, want %q", got, "multipart/m\u0130xed")
	}

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
		_, _ = bw.WriteString("* 1 EXISTS\r\n" + tag + " OK [READ-WRITE] SELECT completed\r\n")
		_ = bw.Flush()
		line, _ = br.ReadString('\n')
		tag, _, _ = strings.Cut(line, " ")
		_, _ = bw.WriteString("* 1 FETCH (BODYSTRUCTURE (\"text\" \"plain\" NIL NIL NIL \"7bit\" 5 1 NIL (\"attachment\" (\"FILENAME\" \"safe.txt\" \"f\u0130lename\" \"spoofed.exe\")) NIL NIL))\r\n")
		_, _ = bw.WriteString(tag + " OK FETCH completed\r\n")
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

	var seqSet imap.SeqSet
	seqSet.AddNum(1)
	msgs, err := c.Fetch(seqSet, &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	bs := msgs[0].BodyStructure.(*imap.BodyStructureSinglePart)
	if got := bs.Filename(); got != "safe.txt" {
		t.Fatalf("Filename() = %q, want %q (U+0130 lookalike overwrote filename param)", got, "safe.txt")
	}
}

func TestServerBodyStructureAndBinaryCTEASCIIFolding(t *testing.T) {
	c := setupMemClientServerForCaseFold(t, false)
	const rawMsg = "From: alice@example.com\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\n=41=42\r\n"
	appendAndSelectInboxForCaseFold(t, c, rawMsg)

	var seqSet imap.SeqSet
	seqSet.AddNum(1)
	msgs, err := c.Fetch(seqSet, &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch BODYSTRUCTURE: %v", err)
	}
	bs := msgs[0].BodyStructure.(*imap.BodyStructureSinglePart)
	if bs.Encoding == "quoted-printable" {
		t.Errorf("BODYSTRUCTURE falsely folded non-ASCII QUOTED-PR\\u0130NTABLE to %q", bs.Encoding)
	} else if bs.Encoding != "quoted-pr\u0130ntable" {
		t.Errorf("BODYSTRUCTURE Encoding = %q, want %q", bs.Encoding, "quoted-pr\u0130ntable")
	}

	_, binErr := c.Fetch(seqSet, &imap.FetchOptions{
		BinarySection: []*imap.FetchItemBinarySection{{Part: []int{1}}},
	}).Collect()
	var imapErr *imap.Error
	if !errors.As(binErr, &imapErr) || imapErr.Code != imap.ResponseCodeUnknownCTE {
		t.Errorf("FETCH BINARY[1] on non-ASCII QUOTED-PR\\u0130NTABLE got err=%v, want NO [UNKNOWN-CTE]", binErr)
	}
}

func TestClientProtocolTokensASCIIFolding(t *testing.T) {
	t.Run("esearch_return_items", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE MULTISEARCH] ready\r\n")
			_ = bw.Flush()
			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString("* 1 EXISTS\r\n" + tag + " OK [READ-WRITE] SELECT completed\r\n")
			_ = bw.Flush()
			line, _ = br.ReadString('\n')
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString("* ESEARCH (TAG \"" + tag + "\") M\u0131N 7 MOD\u017fEQ 99\r\n")
			_, _ = bw.WriteString(tag + " OK SEARCH completed\r\n")
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

		data, err := c.Search(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnMin: true}).Wait()
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if data.Min != 0 || data.ModSeq != 0 {
			t.Fatalf("ESEARCH folded Unicode lookalike return items into Min=%d ModSeq=%d, want 0/0", data.Min, data.ModSeq)
		}
	})

	t.Run("fetch_section_part_mime_lookalike", func(t *testing.T) {
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
			_, _ = bw.WriteString("* 1 EXISTS\r\n" + tag + " OK [READ-WRITE] SELECT completed\r\n")
			_ = bw.Flush()
			line, _ = br.ReadString('\n')
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString("* 1 FETCH (BODY[1.M\u0131ME] \"wrong\" BODY[1.MIME] \"right\")\r\n")
			_, _ = bw.WriteString(tag + " OK FETCH completed\r\n")
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

		var seqSet imap.SeqSet
		seqSet.AddNum(1)
		msgs, err := c.Fetch(seqSet, &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{
				{Part: []int{1}, Specifier: imap.PartSpecifierMIME},
			},
		}).Collect()
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		got := msgs[0].FindBodySection(&imap.FetchItemBodySection{Part: []int{1}, Specifier: imap.PartSpecifierMIME})
		if string(got) != "right" {
			t.Fatalf("FindBodySection(1.MIME) = %q, want %q (matched BODY[1.MıME])", got, "right")
		}
	})

	t.Run("fetch_header_fields_lookalike_rejected", func(t *testing.T) {
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
			_, _ = bw.WriteString("* 1 EXISTS\r\n" + tag + " OK [READ-WRITE] SELECT completed\r\n")
			_ = bw.Flush()
			line, _ = br.ReadString('\n')
			tag, _, _ = strings.Cut(line, " ")
			_, _ = bw.WriteString("* 1 FETCH (BODY[HEADER.FIELD\u017f (Subject)] \"Subject: hi\\r\\n\\r\\n\")\r\n")
			_, _ = bw.WriteString(tag + " OK FETCH completed\r\n")
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

		var seqSet imap.SeqSet
		seqSet.AddNum(1)
		msgs, err := c.Fetch(seqSet, &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject"}},
			},
		}).Collect()
		if err == nil && len(msgs) > 0 && msgs[0].FindBodySection(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject"}}) != nil {
			t.Fatalf("BODY[HEADER.FIELDſ (Subject)] was accepted as HEADER.FIELDS (Subject)")
		}
	})

	t.Run("list_childinfo_lookalike_does_not_hijack_unsolicited_list", func(t *testing.T) {
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		_ = cConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = sConn.SetDeadline(time.Now().Add(5 * time.Second))

		go func() {
			bw := bufio.NewWriter(sConn)
			br := bufio.NewReader(sConn)
			_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 NOTIFY] ready\r\n")
			_ = bw.Flush()
			line, _ := br.ReadString('\n')
			tag, _, _ := strings.Cut(line, " ")
			_, _ = bw.WriteString("* LIST () \"/\" \"Other\" (\"CH\u0131LDINFO\" (\"\u017fUBSCRIBED\"))\r\n")
			_, _ = bw.WriteString("* LIST () \"/\" \"INBOX\"\r\n")
			_, _ = bw.WriteString(tag + " OK LIST completed\r\n")
			_ = bw.Flush()
		}()

		c := imapclient.New(cConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
		defer c.Close()
		if err := c.WaitGreeting(); err != nil {
			t.Fatalf("WaitGreeting: %v", err)
		}

		mailboxes, err := c.List("", "INBOX", nil).Collect()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(mailboxes) != 1 || mailboxes[0].Mailbox != "INBOX" {
			t.Fatalf("List(\"\", \"INBOX\") hijacked unrelated mailbox via CHıLDINFO lookalike: got %d entries (%+v)", len(mailboxes), mailboxes[0])
		}
	})
}

func TestPreviewDispositionASCIIFolding(t *testing.T) {
	c := setupMemClientServerForCaseFold(t, false)
	const rawMsg = "From: alice@example.com\r\nContent-Type: text/plain\r\nContent-Disposition: \u0130nl\u0130ne\r\n\r\nsecret attachment body\r\n"
	appendAndSelectInboxForCaseFold(t, c, rawMsg)

	var seqSet imap.SeqSet
	seqSet.AddNum(1)
	msgs, err := c.Fetch(seqSet, &imap.FetchOptions{
		Preview: &imap.FetchItemPreview{},
	}).Collect()
	if err != nil {
		t.Fatalf("Fetch PREVIEW: %v", err)
	}
	if msgs[0].Preview != nil && *msgs[0].Preview != "" {
		t.Fatalf("PREVIEW leaked text %q from non-ASCII Content-Disposition:\\u0130nl\\u0130ne part", *msgs[0].Preview)
	}
}
