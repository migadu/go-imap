package imapserver_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func TestThreadUnsupportedAlgorithmWithBadCharsetDoesNotEnableCondStore(t *testing.T) {
	c, _, _ := newBadAndByeCondStoreConn(t, false, 4242, false, imap.Cap("THREAD=REFERENCES"))
	c.do("select", "SELECT INBOX")

	resp := c.do("th", "THREAD REFS ISO-8859-1 MODSEQ 1")
	if !strings.Contains(resp, "th BAD") {
		t.Errorf("THREAD REFS ISO-8859-1 MODSEQ 1 = %q, want th BAD", resp)
	}
	if strings.Contains(resp, "[HIGHESTMODSEQ") {
		t.Errorf("unsupported THREAD algorithm with bad charset emitted first-enable HIGHESTMODSEQ: %q", resp)
	}
	if c.srvConn.CondStoreEnabled() {
		t.Errorf("CondStoreEnabled() = true after unsupported THREAD algorithm")
	}
	storeResp := c.do("store", `STORE 1 +FLAGS (\Seen)`)
	if strings.Contains(storeResp, "MODSEQ (") {
		t.Errorf("unsupported THREAD algorithm latched CONDSTORE and leaked unrequested MODSEQ on STORE: %q", storeResp)
	}
}

type customThreadDataSession struct {
	imapserver.Session
	retData []imap.ThreadData
}

func (s *customThreadDataSession) Thread(ctx context.Context, numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	return s.retData, nil
}

func TestThreadPreWriteZeroMessageNumberValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retData []imap.ThreadData
	}{
		{
			name:    "top-level Chain contains 0",
			retData: []imap.ThreadData{{Chain: []uint32{1, 0}}},
		},
		{
			name: "nested SubThreads Chain contains 0",
			retData: []imap.ThreadData{{
				Chain: []uint32{1},
				SubThreads: []imap.ThreadData{
					{Chain: []uint32{2}},
					{Chain: []uint32{0}},
				},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memUser := imapmemserver.NewUser("user", "pass")
			_ = memUser.Create(context.Background(), "INBOX", nil)
			memServer := imapmemserver.New()
			memServer.AddUser(memUser)

			srv := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &customThreadDataSession{
						Session: memServer.NewSession(),
						retData: tc.retData,
					}, nil, nil
				},
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.Cap("THREAD=REFERENCES"): {}},
				InsecureAuth: true,
			})
			defer srv.Close()

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(ln)

			c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			if err := c.Login("user", "pass").Wait(); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}

			_, err = c.Thread(&imapclient.ThreadOptions{
				Algorithm:      imap.ThreadReferences,
				SearchCriteria: &imap.SearchCriteria{},
			}).Wait()
			if err == nil || strings.Contains(err.Error(), "THREAD message numbers start at 1") {
				t.Fatalf("want server NO [SERVERBUG] instead of illegal wire * THREAD with 0, got err=%v", err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatalf("Noop() after server error failed: %v", err)
			}
		})
	}
}

func TestSearchAndESearchUnknownOptionWithNonSyncLiteralStaysInSync(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cmd        string
		wantPrefix string
	}{
		{
			name:       "SEARCH unknown return option followed by non-sync literal in TEXT",
			cmd:        "S1 SEARCH RETURN (FOO) TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "ESEARCH unknown return option followed by non-sync literal in TEXT",
			cmd:        "S1 ESEARCH RETURN (FOO) TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "ESEARCH unknown source option followed by non-sync literal in TEXT",
			cmd:        "S1 ESEARCH IN (foo) TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, imap.CapSet{
				imap.CapIMAP4rev1:       {},
				imap.CapESearch:         {},
				imap.Cap("MULTISEARCH"): {},
				imap.CapLiteralPlus:     {},
			})
			rc.appendOne("search literal sync")
			rc.do("SELECT INBOX")

			if _, err := fmt.Fprint(rc.c, tc.cmd); err != nil {
				t.Fatal(err)
			}
			_ = rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
			line1, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading S1: %v", err)
			}
			if !strings.HasPrefix(line1, tc.wantPrefix) {
				t.Fatalf("want %q, got %q", tc.wantPrefix, strings.TrimRight(line1, "\r\n"))
			}
			line2, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading S2: %v", err)
			}
			if !strings.HasPrefix(line2, "S2 OK ") {
				t.Fatalf("want S2 OK, got %q", strings.TrimRight(line2, "\r\n"))
			}
		})
	}
}
