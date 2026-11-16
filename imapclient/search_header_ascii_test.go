package imapclient_test

import (
	"context"
	"net"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func TestSearchHeaderUnicodeLookalikeNotCollapsedToShorthand(t *testing.T) {
	// U+017F LATIN SMALL LETTER LONG S: strings.ToUpper("ſubject") == "SUBJECT",
	// whereas ASCII case folding keeps "ſUBJECT" distinct from the SUBJECT keyword.
	const lookalikeSubject = "ſubject"

	for _, utf8Accept := range []bool{false, true} {
		name := "rev1"
		if utf8Accept {
			name = "utf8_accept"
		}
		t.Run(name, func(t *testing.T) {
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
				},
				InsecureAuth: true,
			})
			t.Cleanup(func() { srv.Close() })

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			t.Cleanup(func() { ln.Close() })
			go srv.Serve(ln)

			c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			t.Cleanup(func() { c.Close() })

			if err := c.Login("user", "pass").Wait(); err != nil {
				t.Fatalf("Login: %v", err)
			}
			if utf8Accept {
				if _, err := c.Enable(imap.CapUTF8Accept).Wait(); err != nil {
					t.Fatalf("Enable UTF8=ACCEPT: %v", err)
				}
			}

			const rawMsg = "From: alice@example.com\r\nSubject: needle\r\n\r\nbody\r\n"
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

			data, err := c.Search(&imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{{
					Key:   lookalikeSubject,
					Value: "needle",
				}},
			}, nil).Wait()
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got := data.AllSeqNums(); len(got) != 0 {
				t.Fatalf("Search(Header %q) falsely matched messages %v (collapsed into SUBJECT shorthand)", lookalikeSubject, got)
			}
		})
	}
}
