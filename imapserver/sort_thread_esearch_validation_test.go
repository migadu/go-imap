package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

func TestSortUnknownKeyWithNonSyncLiteralStaysInSync(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		wantPrefix string
	}{
		{
			name:       "SORT unknown key followed by non-sync literal in search TEXT",
			cmd:        "S1 SORT (FOO) UTF-8 TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "UID SORT REVERSE unknown key followed by non-sync literal in SUBJECT",
			cmd:        "S1 UID SORT (REVERSE FOO) UTF-8 SUBJECT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "SORT valid then unknown key followed by non-sync literal in HEADER",
			cmd:        "S1 SORT (DATE FOO) UTF-8 HEADER \"Subject\" {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "SORT RETURN (ALL) unknown key followed by non-sync literal in TEXT",
			cmd:        "S1 SORT RETURN (ALL) (FOO) UTF-8 TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "SORT unknown return option followed by non-sync literal in TEXT",
			cmd:        "S1 SORT RETURN (FOO) (DATE) UTF-8 TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "SORT unknown key followed by non-sync literal charset",
			cmd:        "S1 SORT (FOO) {5+}\r\nUTF-8 ALL\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD [CLIENTBUG] ",
		},
		{
			name:       "THREAD unsupported algorithm followed by non-sync literal in TEXT",
			cmd:        "S1 THREAD FOOBAR UTF-8 TEXT {5+}\r\nhello\r\nS2 NOOP\r\n",
			wantPrefix: "S1 BAD ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, imap.CapSet{
				imap.CapIMAP4rev1:             {},
				imap.CapSort:                  {},
				imap.CapESort:                 {},
				imap.Cap("THREAD=REFERENCES"): {},
				imap.CapLiteralPlus:           {},
			})
			rc.appendOne("sort literal sync")
			rc.do("SELECT INBOX")

			if _, err := fmt.Fprint(rc.c, tc.cmd); err != nil {
				t.Fatalf("write failed: %v", err)
			}
			if err := rc.c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatalf("SetReadDeadline: %v", err)
			}

			line1, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading S1 response: %v", err)
			}
			if !strings.HasPrefix(line1, tc.wantPrefix) {
				t.Fatalf("want %q, got %q", tc.wantPrefix, strings.TrimRight(line1, "\r\n"))
			}

			line2, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading S2 response: %v", err)
			}
			if !strings.HasPrefix(line2, "S2 OK ") {
				t.Fatalf("want S2 OK, got %q", strings.TrimRight(line2, "\r\n"))
			}
		})
	}
}

type sortThreadCondStoreSession struct {
	imapserver.Session
}

func (s *sortThreadCondStoreSession) HighestModSeq() uint64 {
	return firstEnableHighestModSeq
}

func (s *sortThreadCondStoreSession) Thread(ctx context.Context, numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	return s.Session.(imapserver.SessionThread).Thread(ctx, numKind, algorithm, charset, criteria)
}

func newSortThreadCondStoreConn(t *testing.T) *storeTestConn {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &sortThreadCondStoreSession{Session: mem.NewSession()}, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:             {},
			imap.CapCondStore:             {},
			imap.CapSort:                  {},
			imap.Cap("THREAD=REFERENCES"): {},
			imap.CapLiteralPlus:           {},
		},
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	c.do("login", "LOGIN user pass")
	c.appendMessage("append1")
	return c
}

func TestSortAndThreadBadCharsetAnnouncesCondStoreAndSyncsLiterals(t *testing.T) {
	t.Run("BADCHARSET_MODSEQ_announces_CONDSTORE_like_SEARCH", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cmd  string
		}{
			{"SORT_BADCHARSET_MODSEQ", "SORT (DATE) ISO-8859-1 MODSEQ 1"},
			{"THREAD_BADCHARSET_MODSEQ", "THREAD REFERENCES ISO-8859-1 MODSEQ 1"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := newSortThreadCondStoreConn(t)
				c.do("sel", "SELECT INBOX")

				resp := c.do("fail", tc.cmd)
				if !strings.Contains(resp, "fail NO [BADCHARSET]") {
					t.Fatalf("%s = %q, want NO [BADCHARSET]", tc.cmd, resp)
				}
				if !strings.Contains(resp, "* OK [HIGHESTMODSEQ 4242]") {
					t.Errorf("valid enabling command %q answered NO [BADCHARSET] missed first-enable HIGHESTMODSEQ: %q", tc.cmd, resp)
				}

				storeResp := c.do("store", `STORE 1 +FLAGS (\Flagged)`)
				if !strings.Contains(storeResp, "MODSEQ (") {
					t.Errorf("STORE after valid enabling %q answered NO omitted mandatory MODSEQ: %q", tc.cmd, storeResp)
				}
			})
		}
	})

	t.Run("BADCHARSET_with_nonsync_literal_stays_in_sync", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			wire string
		}{
			{
				name: "SORT",
				wire: "s1 SORT (DATE) ISO-8859-1 SUBJECT {5+}\r\nhello MODSEQ 1\r\nn1 NOOP\r\n",
			},
			{
				name: "THREAD",
				wire: "s1 THREAD REFERENCES ISO-8859-1 SUBJECT {5+}\r\nhello MODSEQ 1\r\nn1 NOOP\r\n",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := newSortThreadCondStoreConn(t)
				c.do("sel", "SELECT INBOX")

				if _, err := fmt.Fprint(c.conn, tc.wire); err != nil {
					t.Fatal(err)
				}
				var sb strings.Builder
				for {
					line, err := c.br.ReadString('\n')
					sb.WriteString(line)
					if strings.HasPrefix(line, "n1 ") {
						break
					}
					if err != nil {
						if err == io.EOF {
							break
						}
						t.Fatalf("read error: %v (got so far: %q)", err, sb.String())
					}
				}
				got := sb.String()
				if strings.Contains(got, "* BYE") {
					t.Fatalf("valid %s with unsupported charset and LITERAL+ desynchronized and dropped connection:\n%s", tc.name, got)
				}
				if !strings.Contains(got, "s1 NO [BADCHARSET]") || !strings.Contains(got, "n1 OK") {
					t.Fatalf("unexpected pipelined response:\n%s", got)
				}
			})
		}
	})
}

type multiSearchMissingMailboxSession struct {
	imapserver.Session
}

func (s *multiSearchMissingMailboxSession) MultiSearch(ctx context.Context, source *imap.SearchSource, criteria *imap.SearchCriteria, options *imap.SearchOptions) ([]*imap.SearchData, error) {
	return []*imap.SearchData{
		{Mailbox: "INBOX", UIDValidity: 1, All: imap.UIDSetNum(1)},
		{Mailbox: "", UIDValidity: 0, All: imap.UIDSetNum(2)},
	}, nil
}

type customSortDataSession struct {
	imapserver.Session
	retData *imap.SortData
}

func (s *customSortDataSession) Sort(ctx context.Context, numKind imapserver.NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	return s.retData, nil
}

func TestESearchAndSortPreWriteValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retAll imap.NumSet
	}{
		{
			name:   "Start==0 with non-zero Stop",
			retAll: imap.SeqSet{imap.SeqRange{Start: 0, Stop: 5}},
		},
		{
			name:   "dynamic range before final static element",
			retAll: imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}, imap.SeqRange{Start: 5, Stop: 5}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memUser := imapmemserver.NewUser("user", "pass")
			_ = memUser.Create(context.Background(), "INBOX", nil)
			memServer := imapmemserver.New()
			memServer.AddUser(memUser)

			srv := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return &customSearchSession{
						Session: memServer.NewSession(),
						retAll:  tc.retAll,
					}, nil, nil
				},
				Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}},
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
			_, err = c.Search(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true}).Wait()
			if err == nil || strings.Contains(err.Error(), "dynamic ALL number set") {
				t.Fatalf("want server NO [SERVERBUG] instead of illegal wire ESEARCH ALL, got err=%v", err)
			}
		})
	}

	t.Run("MultiSearch result missing Mailbox fails before writing first mailbox ESEARCH", func(t *testing.T) {
		caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapESearch: {}, imap.Cap("MULTISEARCH"): {}}
		rc := newSearchRev2ConnWrapped(t, caps, func(s imapserver.Session) imapserver.Session {
			return &multiSearchMissingMailboxSession{Session: s}
		})
		rc.appendOne("msg")
		rc.do("SELECT INBOX")

		resp, tagged := rc.try("ESEARCH IN (personal) ALL")
		if !strings.Contains(tagged, " NO [SERVERBUG] ") || resp != tagged {
			t.Fatalf("want only tagged NO [SERVERBUG] with no partial untagged ESEARCH, got:\n%s", resp)
		}
	})

	t.Run("SORT RETURN (ALL) with nil SortData returns NO SERVERBUG without panicking", func(t *testing.T) {
		caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}, imap.CapESort: {}}
		rc := newSearchRev2ConnWrapped(t, caps, func(s imapserver.Session) imapserver.Session {
			return &customSortDataSession{Session: s, retData: nil}
		})
		rc.appendOne("msg")
		rc.do("SELECT INBOX")

		if _, err := rc.c.Write([]byte("t99 SORT RETURN (ALL) (DATE) UTF-8 ALL\r\n")); err != nil {
			t.Fatal(err)
		}
		line, err := rc.br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "t99 NO [SERVERBUG] ") {
			t.Fatalf("SORT with nil SortData got err=%v line=%q, want t99 NO [SERVERBUG]", err, line)
		}
	})

	t.Run("SORT RETURN (ALL) with 0 in SortData.All returns NO SERVERBUG", func(t *testing.T) {
		caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSort: {}, imap.CapESort: {}}
		rc := newSearchRev2ConnWrapped(t, caps, func(s imapserver.Session) imapserver.Session {
			return &customSortDataSession{Session: s, retData: &imap.SortData{All: []uint32{1, 0}}}
		})
		rc.appendOne("msg")
		rc.do("SELECT INBOX")

		resp, tagged := rc.try("SORT RETURN (ALL) (DATE) UTF-8 ALL")
		if !strings.Contains(tagged, " NO [SERVERBUG] ") || resp != tagged {
			t.Fatalf("SORT with 0 in SortData.All: want NO [SERVERBUG], got:\n%s", resp)
		}
	})
}
