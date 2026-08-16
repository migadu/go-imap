package imapserver_test

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// searchRev2Conn is a raw IMAP client. The response FORM is what these tests
// are about, so they read bytes rather than going through imapclient, which
// would report what it managed to parse instead of what arrived.
type searchRev2Conn struct {
	t   *testing.T
	c   net.Conn
	br  *bufio.Reader
	seq int
}

func newSearchRev2Conn(t *testing.T, caps imap.CapSet) *searchRev2Conn {
	t.Helper()
	return newSearchRev2ConnWrapped(t, caps, nil)
}

// newSearchRev2ConnWrapped is newSearchRev2Conn with a hook that can decorate
// the backend session, so a test can drive a writer the in-memory backend never
// exercises on its own.
func newSearchRev2ConnWrapped(t *testing.T, caps imap.CapSet, wrap func(imapserver.Session) imapserver.Session) *searchRev2Conn {
	t.Helper()

	const username, password = "user", "pass"
	memUser := imapmemserver.NewUser(username, password)
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create INBOX: %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			sess := memServer.NewSession()
			if wrap != nil {
				sess = wrap(sess)
			}
			return sess, nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	rc := &searchRev2Conn{t: t, c: conn, br: bufio.NewReader(conn)}
	if _, err := rc.br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}
	rc.do("LOGIN " + username + " " + password)
	return rc
}

// do sends one command and returns the whole response, tagged line included.
func (rc *searchRev2Conn) do(cmd string) string {
	rc.t.Helper()
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	if _, err := rc.c.Write([]byte(tag + " " + cmd + "\r\n")); err != nil {
		rc.t.Fatalf("write %q: %v", cmd, err)
	}
	var buf bytes.Buffer
	for {
		line, err := rc.br.ReadString('\n')
		buf.WriteString(line)
		if strings.HasPrefix(line, tag+" ") {
			resp := buf.String()
			if !strings.Contains(line, " OK ") {
				rc.t.Fatalf("%s → %s", cmd, strings.TrimRight(line, "\r\n"))
			}
			return resp
		}
		if err != nil {
			rc.t.Fatalf("reading response to %q: %v (got %q)", cmd, err, buf.String())
		}
	}
}

// appendOne stores one trivial message in INBOX.
func (rc *searchRev2Conn) appendOne(subject string) {
	rc.t.Helper()
	body := "Subject: " + subject + "\r\n\r\nbody\r\n"
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	cmd := tag + " APPEND INBOX {" + strconv.Itoa(len(body)) + "}\r\n"
	if _, err := rc.c.Write([]byte(cmd)); err != nil {
		rc.t.Fatalf("APPEND: %v", err)
	}
	line, err := rc.br.ReadString('\n') // continuation request
	if err != nil {
		rc.t.Fatalf("APPEND continuation: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		rc.t.Fatalf("expected a continuation request, got %q", line)
	}
	if _, err := rc.c.Write([]byte(body + "\r\n")); err != nil {
		rc.t.Fatalf("APPEND literal: %v", err)
	}
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			rc.t.Fatalf("APPEND completion: %v", err)
		}
		if strings.HasPrefix(line, tag+" ") {
			if !strings.Contains(line, " OK ") {
				rc.t.Fatalf("APPEND → %s", strings.TrimRight(line, "\r\n"))
			}
			return
		}
	}
}

func dualStackCaps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapIMAP4rev2: {},
	}
}

// A session that has enabled IMAP4rev2 must receive the ESEARCH response even
// for a SEARCH with no RETURN clause.
//
// RFC 9051 §6.4.4 lists ESEARCH as SEARCH's only untagged response and assumes
// ALL when no result option is given; Appendix E item 4 records the change as
// "SEARCH command now requires to return the ESEARCH response (SEARCH response
// is now deprecated)".
func TestSearchRev2PlainSearchReturnsESearch(t *testing.T) {
	rc := newSearchRev2Conn(t, dualStackCaps())
	rc.appendOne("one")
	rc.appendOne("two")
	rc.do("ENABLE IMAP4rev2")
	rc.do("SELECT INBOX")

	resp := rc.do("SEARCH ALL")
	if strings.Contains(resp, "* SEARCH ") {
		t.Errorf("rev2 plain SEARCH answered the deprecated SEARCH response:\n%s", resp)
	}
	if !strings.Contains(resp, "* ESEARCH ") {
		t.Fatalf("rev2 plain SEARCH did not answer ESEARCH:\n%s", resp)
	}
	// ALL is assumed when no result option is given (§6.4.4), so the matches
	// must actually be in the response — an item-less ESEARCH would be a
	// different bug wearing the right response name.
	if !strings.Contains(resp, "ALL 1:2") {
		t.Errorf("expected ALL 1:2 in the ESEARCH response:\n%s", resp)
	}
}

// UID SEARCH takes the same path and must carry the UID indicator (§7.3.4),
// without which the client cannot tell UIDs from message numbers.
func TestSearchRev2PlainUIDSearchReturnsESearchWithUID(t *testing.T) {
	rc := newSearchRev2Conn(t, dualStackCaps())
	rc.appendOne("one")
	rc.do("ENABLE IMAP4rev2")
	rc.do("SELECT INBOX")

	resp := rc.do("UID SEARCH ALL")
	if !strings.Contains(resp, "* ESEARCH ") {
		t.Fatalf("rev2 plain UID SEARCH did not answer ESEARCH:\n%s", resp)
	}
	if !strings.Contains(resp, " UID ") {
		t.Errorf("ESEARCH for UID SEARCH lacks the UID indicator:\n%s", resp)
	}
}

// The rev1 wire form is untouched. This is the half that makes the change safe
// to ship on a dual-stack server: the same connection would have received
// ESEARCH had it sent ENABLE, and does not because it did not. Gating on the
// ENABLED set rather than the advertised one is what buys that.
func TestSearchRev1PlainSearchKeepsSearchResponse(t *testing.T) {
	rc := newSearchRev2Conn(t, dualStackCaps())
	rc.appendOne("one")
	rc.do("SELECT INBOX") // deliberately no ENABLE

	resp := rc.do("SEARCH ALL")
	if strings.Contains(resp, "* ESEARCH") {
		t.Errorf("rev1 session received ESEARCH for a plain SEARCH:\n%s", resp)
	}
	if !strings.Contains(resp, "* SEARCH 1") {
		t.Fatalf("rev1 plain SEARCH lost the SEARCH response:\n%s", resp)
	}
}

// An extended SEARCH is unchanged in both revisions — the RFC 4731 path this
// change does not touch.
func TestSearchExtendedReturnsESearchInBothRevisions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
	}{
		{"rev1", false},
		{"rev2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, dualStackCaps())
			rc.appendOne("one")
			if tc.enable {
				rc.do("ENABLE IMAP4rev2")
			}
			rc.do("SELECT INBOX")

			resp := rc.do("SEARCH RETURN (COUNT) ALL")
			if !strings.Contains(resp, "* ESEARCH ") {
				t.Fatalf("extended SEARCH did not answer ESEARCH:\n%s", resp)
			}
			if !strings.Contains(resp, "COUNT 1") {
				t.Errorf("expected COUNT 1:\n%s", resp)
			}
		})
	}
}

// A rev2 search with no matches still gets an ESEARCH response, carrying no
// ALL item: §7.3.4 says the server "MUST NOT include the ALL return item …
// however, it still MUST send the ESEARCH response". Worth pinning separately,
// because the natural way to write the empty case is to send nothing at all.
func TestSearchRev2PlainSearchNoMatchesStillSendsESearch(t *testing.T) {
	rc := newSearchRev2Conn(t, dualStackCaps())
	rc.appendOne("one")
	rc.do("ENABLE IMAP4rev2")
	rc.do("SELECT INBOX")

	resp := rc.do(`SEARCH SUBJECT "nothing matches this"`)
	if !strings.Contains(resp, "* ESEARCH") {
		t.Fatalf("empty rev2 search sent no ESEARCH response:\n%s", resp)
	}
	if strings.Contains(resp, " ALL ") {
		t.Errorf("empty rev2 search included an ALL item:\n%s", resp)
	}
}

// A server advertising IMAP4rev2 WITHOUT IMAP4rev1 is rev2-only: there are no
// legacy clients to protect, so every session is an IMAP4rev2 session whether
// or not it sent ENABLE, and the deprecated SEARCH response must never appear.
//
// The no-ENABLE case is the one that matters and the one that regressed: the
// first version of this fix gated only on the ENABLED set, and an earlier
// version of this very test sent ENABLE before asserting -- so it passed while
// the case it named went untested. Both variants now run.
func TestSearchRev2OnlyServerNeverSendsSearchResponse(t *testing.T) {
	for _, enable := range []bool{false, true} {
		name := "without ENABLE"
		if enable {
			name = "with ENABLE"
		}
		t.Run(name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev2: {}})
			rc.appendOne("one")
			if enable {
				rc.do("ENABLE IMAP4rev2")
			}
			rc.do("SELECT INBOX")

			resp := rc.do("SEARCH ALL")
			if strings.Contains(resp, "* SEARCH ") {
				t.Errorf("rev2-only server answered the deprecated SEARCH response:\n%s", resp)
			}
			if !strings.Contains(resp, "* ESEARCH") {
				t.Fatalf("rev2-only server did not answer ESEARCH:\n%s", resp)
			}
		})
	}
}

// The rev2-only server's response forms must agree ACROSS commands, which is
// what the shared isIMAP4rev2 helper buys. Before it, each command open-coded
// the test in its own spelling: SELECT and STATUS honoured the rev2-only case
// while SEARCH did not, so one connection could be told it was rev2 by one
// command and rev1 by the next.
//
// No ENABLE anywhere here -- that is the whole point.
func TestRev2OnlyServerIsConsistentAcrossCommands(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev2: {}})
	rc.appendOne("one")

	sel := rc.do("SELECT INBOX")
	if strings.Contains(sel, " RECENT") {
		t.Errorf("rev2-only SELECT sent the deprecated RECENT response:\n%s", sel)
	}
	if strings.Contains(sel, "[UNSEEN ") {
		t.Errorf("rev2-only SELECT sent the deprecated UNSEEN response code:\n%s", sel)
	}

	if !strings.Contains(rc.do("SEARCH ALL"), "* ESEARCH") {
		t.Error("rev2-only SEARCH did not answer ESEARCH")
	}

	// STATUS (RECENT) is not a rev2 item and must be refused, not answered 0.
	rc.seq++
	tag := "t" + strconv.Itoa(rc.seq)
	if _, err := rc.c.Write([]byte(tag + " STATUS INBOX (RECENT)\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.HasPrefix(line, tag+" ") {
			if strings.Contains(line, " OK ") {
				t.Errorf("rev2-only STATUS (RECENT) was accepted: %s", strings.TrimRight(line, "\r\n"))
			}
			break
		}
	}
}

func TestRenameRev2OnlyServerSendsOldNameWithoutEnable(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{imap.CapIMAP4rev2: {}})
	rc.do("CREATE oldbox")
	resp := rc.do("RENAME oldbox newbox")
	if !strings.Contains(resp, "OLDNAME") {
		t.Fatalf("rev2-only RENAME did not send OLDNAME:\n%s", resp)
	}
}

// oldNamePollSession emits one unsolicited LIST carrying OLDNAME from Poll.
// That is the NON-NOTIFY branch of UpdateWriter.WriteList, which the in-memory
// backend never drives on its own.
// It embeds SessionIMAP4rev2, not Session: a rev2-only server panics on a
// session that does not implement it, since there would be no usable base
// protocol left.
type oldNamePollSession struct {
	imapserver.SessionIMAP4rev2
}

// Emitted on EVERY poll, not once: the server polls at each command sync point
// (SELECT included), so a one-shot probe would be spent before the command the
// assertion reads.
func (s *oldNamePollSession) Poll(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if err := s.SessionIMAP4rev2.Poll(ctx, w, allowExpunge); err != nil {
		return err
	}
	return w.WriteList(&imap.ListData{Mailbox: "newbox", Delim: '/', OldName: "oldbox"})
}

// UpdateWriter.WriteList carries the same rev2-only hazard RENAME's writer did:
// gating OLDNAME on the ENABLED set alone strips it on a server that advertises
// no IMAP4rev1, where every session is a rev2 session without any ENABLE. The
// NOTIFY branch is unaffected (it emits OLDNAME regardless), so this drives the
// non-NOTIFY path through Poll.
func TestUpdateWriterRev2OnlyServerSendsOldNameWithoutEnable(t *testing.T) {
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev2: {}},
		func(s imapserver.Session) imapserver.Session {
			return &oldNamePollSession{SessionIMAP4rev2: s.(imapserver.SessionIMAP4rev2)}
		})
	rc.do("CREATE oldbox")
	rc.do("SELECT INBOX")
	resp := rc.do("NOOP")
	if !strings.Contains(resp, "OLDNAME") {
		t.Fatalf("rev2-only unsolicited LIST did not carry OLDNAME:\n%s", resp)
	}
}
