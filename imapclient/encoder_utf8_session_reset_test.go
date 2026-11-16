package imapclient_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type unauthUTF8MemSession struct {
	mem *imapmemserver.Server
	imapserver.SessionIMAP4rev2
}

var _ imapserver.SessionUnauthenticate = (*unauthUTF8MemSession)(nil)

func (s *unauthUTF8MemSession) Unauthenticate(ctx context.Context) error {
	if s.SessionIMAP4rev2 != nil {
		_ = s.SessionIMAP4rev2.Close()
	}
	s.SessionIMAP4rev2 = s.mem.NewSession().(imapserver.SessionIMAP4rev2)
	return nil
}

// RFC 8437 §2 resets enabled extensions on UNAUTHENTICATE. Commands pipelined
// after c.Unauthenticate() will be parsed by the server after UNAUTHENTICATE
// resets enabled extensions, so beginCommand must not inherit the prior user's
// UTF8=ACCEPT / IMAP4rev2 encoder mode, while c.dec.QuotedUTF8 must remain
// enabled until UNAUTHENTICATE's tagged OK arrives.
func TestPipelinedUnauthenticateResetsEncoderQuotedUTF8(t *testing.T) {
	for _, enableCap := range []imap.Cap{imap.CapUTF8Accept, imap.CapIMAP4rev2} {
		t.Run(string(enableCap)+"/imapserver_select_and_delete", func(t *testing.T) {
			mem := imapmemserver.New()

			alice := imapmemserver.NewUser("alice", "secret")
			if err := alice.Create(context.Background(), "INBOX", nil); err != nil {
				t.Fatal(err)
			}
			mem.AddUser(alice)

			bob := imapmemserver.NewUser("bob", "secret")
			for _, mbox := range []string{"INBOX", "R&D", "A&B", "A&-B"} {
				if err := bob.Create(context.Background(), mbox, nil); err != nil {
					t.Fatal(err)
				}
			}
			mem.AddUser(bob)

			srv := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &unauthUTF8MemSession{
						mem:              mem,
						SessionIMAP4rev2: mem.NewSession().(imapserver.SessionIMAP4rev2),
					}, nil, nil
				},
				Caps: imap.CapSet{
					imap.CapIMAP4rev1:      {},
					imap.CapIMAP4rev2:      {},
					imap.CapEnable:         {},
					imap.CapUTF8Accept:     {},
					imap.CapUnauthenticate: {},
					imap.CapLiteralPlus:    {},
				},
				InsecureAuth: true,
			})
			defer srv.Close()

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go srv.Serve(ln)

			c, err := imapclient.DialInsecure(ln.Addr().String(), &imapclient.Options{ResponseTimeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			if err := c.Login("alice", "secret").Wait(); err != nil {
				t.Fatalf("alice Login: %v", err)
			}
			enabledData, err := c.Enable(enableCap).Wait()
			if err != nil {
				t.Fatalf("alice Enable(%s): %v", enableCap, err)
			}
			if !enabledData.Caps.Has(enableCap) {
				t.Fatalf("alice Enable(%s) = %v, want %s enabled", enableCap, enabledData.Caps, enableCap)
			}

			unauth := c.Unauthenticate()
			loginBob := c.Login("bob", "secret")
			del := c.Delete("A&-B")
			sel := c.Select("R&D", nil)

			if err := unauth.Wait(); err != nil {
				t.Fatalf("Unauthenticate: %v", err)
			}
			if err := loginBob.Wait(); err != nil {
				t.Fatalf("bob Login: %v", err)
			}
			if err := del.Wait(); err != nil {
				t.Fatalf("bob Delete(\"A&-B\"): %v", err)
			}
			if _, err := sel.Wait(); err != nil {
				t.Errorf("bob pipelined Select(\"R&D\") failed against imapserver: %v", err)
			}

			mailboxes, err := c.List("", "*", nil).Collect()
			if err != nil {
				t.Fatalf("bob List: %v", err)
			}
			got := make(map[string]bool)
			for _, m := range mailboxes {
				got[m.Mailbox] = true
			}
			if !got["A&B"] {
				t.Errorf("bob's mailbox \"A&B\" was silently deleted when Delete(\"A&-B\") leaked QuotedUTF8 across UNAUTHENTICATE (remaining: %v)", got)
			}
			if got["A&-B"] {
				t.Errorf("bob's mailbox \"A&-B\" was NOT deleted (remaining: %v)", got)
			}
		})
	}

	t.Run("wire_login_and_search_after_unauthenticate_while_preserving_pre_unauth_decoder", func(t *testing.T) {
		const caps = "IMAP4rev1 ENABLE UTF8=ACCEPT UNAUTHENTICATE LITERAL+"
		var preUnauthList []string
		wireCmds := make(chan string, 8)

		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
		_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

		releaseUnauth := make(chan struct{})
		go func() {
			_, _ = io.WriteString(serverConn, "* PREAUTH [CAPABILITY "+caps+"] ready\r\n")
			br := bufio.NewReader(serverConn)

			readFullCmd := func() (tag, rest string, ok bool) {
				line, err := br.ReadString('\n')
				if err != nil {
					return "", "", false
				}
				line = strings.TrimSuffix(line, "\r\n")
				tag, rest, _ = strings.Cut(line, " ")
				for strings.HasSuffix(rest, "+}") {
					idx := strings.LastIndex(rest, "{")
					if idx < 0 {
						break
					}
					n, err := strconv.Atoi(rest[idx+1 : len(rest)-2])
					if err != nil {
						break
					}
					lit := make([]byte, n)
					if _, err := io.ReadFull(br, lit); err != nil {
						return "", "", false
					}
					cont, err := br.ReadString('\n')
					if err != nil {
						return "", "", false
					}
					cont = strings.TrimSuffix(cont, "\r\n")
					rest += "\r\n" + string(lit) + cont
				}
				return tag, rest, true
			}

			tag, _, ok := readFullCmd()
			if !ok {
				return
			}
			_, _ = io.WriteString(serverConn, "* ENABLED UTF8=ACCEPT\r\n"+tag+" OK enabled\r\n")

			unauthTag, _, ok := readFullCmd()
			if !ok {
				return
			}
			loginTag, loginCmd, ok := readFullCmd()
			if !ok {
				return
			}
			wireCmds <- loginCmd
			selTag, selCmd, ok := readFullCmd()
			if !ok {
				return
			}
			wireCmds <- selCmd
			searchTag, searchCmd, ok := readFullCmd()
			if !ok {
				return
			}
			wireCmds <- searchCmd

			<-releaseUnauth
			_, _ = io.WriteString(serverConn, "* LIST () \"/\" \"R&D\"\r\n")
			_, _ = io.WriteString(serverConn, unauthTag+" OK [CAPABILITY "+caps+"] unauthenticated\r\n")
			_, _ = io.WriteString(serverConn, loginTag+" OK [CAPABILITY "+caps+"] logged in\r\n")
			_, _ = io.WriteString(serverConn, "* 1 EXISTS\r\n"+selTag+" OK selected\r\n")
			_, _ = io.WriteString(serverConn, "* SEARCH\r\n"+searchTag+" OK search done\r\n")
		}()

		c := imapclient.New(clientConn, &imapclient.Options{
			ResponseTimeout: 5 * time.Second,
			UnilateralDataHandler: &imapclient.UnilateralDataHandler{
				List: func(d *imap.ListData) {
					preUnauthList = append(preUnauthList, d.Mailbox)
				},
			},
		})
		defer c.Close()

		if _, err := c.Enable(imap.CapUTF8Accept).Wait(); err != nil {
			t.Fatal(err)
		}

		unauth := c.Unauthenticate()
		login := c.Login("bob", "päss")
		sel := c.Select("café", nil)
		srch := c.UIDSearch(&imap.SearchCriteria{Body: []string{"café"}}, nil)
		close(releaseUnauth)

		if err := unauth.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := login.Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := sel.Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := srch.Wait(); err != nil {
			t.Fatal(err)
		}

		gotLogin := <-wireCmds
		gotSelect := <-wireCmds
		gotSearch := <-wireCmds

		if len(preUnauthList) != 1 || preUnauthList[0] != "R&D" {
			t.Errorf("pre-UNAUTHENTICATE unsolicited UTF-8 LIST = %q, want [\"R&D\"]", preUnauthList)
		}
		if gotLogin != "LOGIN bob {5+}\r\npäss" && gotLogin != "LOGIN \"bob\" {5+}\r\npäss" {
			t.Errorf("pipelined post-UNAUTHENTICATE LOGIN sent raw 8-bit quoted UTF-8: %q", gotLogin)
		}
		if gotSelect != "SELECT \"caf&AOk-\"" && gotSelect != "SELECT caf&AOk-" {
			t.Errorf("pipelined post-UNAUTHENTICATE SELECT sent raw UTF-8 instead of MUTF-7: %q", gotSelect)
		}
		if gotSearch != "UID SEARCH CHARSET UTF-8 BODY {5+}\r\ncafé" {
			t.Errorf("pipelined post-UNAUTHENTICATE UID SEARCH = %q, want %q", gotSearch, "UID SEARCH CHARSET UTF-8 BODY {5+}\r\ncafé")
		}
	})
}

// RFC 6855 §3 forbids CHARSET in SEARCH once UTF8=ACCEPT is enabled, while
// IMAP4rev1 without UTF8=ACCEPT forbids raw 8-bit quoted UTF-8. Sampling
// QuotedUTF8 once on the commandEncoder ensures CHARSET and search keys never
// split across concurrent * ENABLED UTF8=ACCEPT arrival.
func TestSearchCharsetConsistentWithEncoderQuotedUTF8(t *testing.T) {
	for _, kind := range []string{"UIDSearch", "MultiSearch"} {
		t.Run(kind, func(t *testing.T) {
			const caps = "IMAP4rev1 ENABLE UTF8=ACCEPT LITERAL+ MULTISEARCH"
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

			appendReceived := make(chan struct{})
			sendEnabled := make(chan struct{})
			sendContinue := make(chan struct{})
			searchWire := make(chan string, 1)

			go func() {
				_, _ = io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 ENABLE UTF8=ACCEPT MULTISEARCH] ready\r\n")
				br := bufio.NewReader(serverConn)

				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				selTag, _, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
				_, _ = io.WriteString(serverConn, "* 1 EXISTS\r\n"+selTag+" OK selected\r\n")

				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				enableTag, _, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")

				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				appendTag, _, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
				close(appendReceived)

				<-sendEnabled
				_, _ = io.WriteString(serverConn, "* ENABLED UTF8=ACCEPT\r\n"+enableTag+" OK [CAPABILITY "+caps+"] enabled\r\n")

				<-sendContinue
				_, _ = io.WriteString(serverConn, "+ go ahead\r\n")

				var buf [6]byte
				if _, err := io.ReadFull(br, buf[:]); err != nil {
					return
				}
				_, _ = io.WriteString(serverConn, appendTag+" OK appended\r\n")

				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSuffix(line, "\r\n")
				searchTag, rest, _ := strings.Cut(line, " ")
				searchWire <- rest
				_, _ = io.WriteString(serverConn, searchTag+" OK done\r\n")
			}()

			c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 5 * time.Second})
			defer c.Close()

			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}

			enableCmd := c.Enable(imap.CapUTF8Accept)

			appendDone := make(chan error, 1)
			go func() {
				app := c.Append("INBOX", 4, nil)
				_, _ = app.Write([]byte("test"))
				_ = app.Close()
				_, err := app.Wait()
				appendDone <- err
			}()

			<-appendReceived
			searchDone := make(chan error, 1)
			go func() {
				if kind == "UIDSearch" {
					_, err := c.UIDSearch(&imap.SearchCriteria{Body: []string{"café"}}, nil).Wait()
					searchDone <- err
				} else {
					_, err := c.MultiSearch(nil, &imap.SearchCriteria{Body: []string{"café"}}, nil).Wait()
					searchDone <- err
				}
			}()

			time.Sleep(15 * time.Millisecond)
			close(sendEnabled)
			if _, err := enableCmd.Wait(); err != nil {
				t.Fatal(err)
			}
			close(sendContinue)

			if err := <-appendDone; err != nil {
				t.Fatal(err)
			}
			if err := <-searchDone; err != nil {
				t.Fatal(err)
			}

			got := <-searchWire
			if strings.Contains(got, "CHARSET UTF-8") && strings.Contains(got, "\"café\"") {
				t.Fatalf("split-brain %s emitted forbidden CHARSET UTF-8 together with raw UTF-8 quoted string (RFC 6855 §3): %q", kind, got)
			}
		})
	}
}
