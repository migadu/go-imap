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
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// quotaTestSession is the imapmemserver session, which the wrappers below
// embed to change one aspect of its QUOTA support.
type quotaTestSession interface {
	imapserver.SessionIMAP4rev2
	imapserver.SessionQuota
}

// nilQuotaSession returns no data and no error from GetQuota.
type nilQuotaSession struct{ quotaTestSession }

func (nilQuotaSession) GetQuota(ctx context.Context, root string) (*imap.QuotaData, error) {
	return nil, nil
}

// quotaTypesSession returns the given resource types.
type quotaTypesSession struct {
	quotaTestSession
	types []imap.QuotaResourceType
}

func (s quotaTypesSession) QuotaResourceTypes() []imap.QuotaResourceType {
	return s.types
}

// quotaCapsSession has a per-session capability set without QUOTA.
type quotaCapsSession struct{ quotaTestSession }

func (quotaCapsSession) GetCapabilities() imap.CapSet {
	return imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
}

// rawQuotaConn is a logged in raw connection to an imapmemserver whose user
// has testQuotaLimits and an INBOX.
type rawQuotaConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func newRawQuotaConn(t *testing.T, wrap func(quotaTestSession) imapserver.Session) *rawQuotaConn {
	t.Helper()
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testUsername, testPassword)
	user.Create(context.Background(), "INBOX", nil)
	user.SetQuota(testQuotaLimits)
	memServer.AddUser(user)

	conn, server := newMemServerConn(t, func() imapserver.Session {
		return wrap(memServer.NewSession().(quotaTestSession))
	})
	t.Cleanup(func() {
		conn.Close()
		server.Close()
	})

	r := &rawQuotaConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	r.readLine() // greeting
	if got := r.cmd("A0", "LOGIN "+testUsername+" "+testPassword); !strings.HasPrefix(got[len(got)-1], "A0 OK") {
		t.Fatalf("LOGIN = %q", got)
	}
	return r
}

func (r *rawQuotaConn) readLine() string {
	r.t.Helper()
	r.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := r.br.ReadString('\n')
	if err != nil {
		r.t.Fatalf("ReadString() = %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// cmd sends a command and returns the response lines up to the tagged one.
func (r *rawQuotaConn) cmd(tag, s string) []string {
	r.t.Helper()
	if _, err := io.WriteString(r.conn, tag+" "+s+"\r\n"); err != nil {
		r.t.Fatalf("WriteString() = %v", err)
	}
	var lines []string
	for {
		line := r.readLine()
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines
		}
	}
}

// capabilities returns the CAPABILITY response tokens.
func (r *rawQuotaConn) capabilities() []string {
	r.t.Helper()
	lines := r.cmd("C1", "CAPABILITY")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "* CAPABILITY ") {
		r.t.Fatalf("CAPABILITY = %q", lines)
	}
	return strings.Fields(strings.TrimPrefix(lines[0], "* CAPABILITY "))
}

func quotaCaps(caps []string) []string {
	var l []string
	for _, c := range caps {
		if strings.HasPrefix(c, "QUOTA") {
			l = append(l, c)
		}
	}
	return l
}

func TestQuota_nilData(t *testing.T) {
	r := newRawQuotaConn(t, func(s quotaTestSession) imapserver.Session {
		return nilQuotaSession{s}
	})
	if got := r.cmd("A1", `GETQUOTA ""`); len(got) != 1 || !strings.HasPrefix(got[0], "A1 NO [SERVERBUG]") {
		t.Errorf("GETQUOTA = %q, want only NO [SERVERBUG]", got)
	}
	if got := r.cmd("A2", "NOOP"); !strings.HasPrefix(got[len(got)-1], "A2 OK") {
		t.Errorf("NOOP = %q, want OK", got)
	}
}

func TestQuota_resourceTypes(t *testing.T) {
	tests := []struct {
		name     string
		types    []imap.QuotaResourceType
		wantCaps []string // nil: QUOTA is not offered
		wantList string
	}{
		{name: "none"},
		{name: "only invalid", types: []imap.QuotaResourceType{"", "FOO BAR", "KVÓTA", "A)"}},
		{
			name:     "repeated",
			types:    []imap.QuotaResourceType{imap.QuotaResourceStorage, imap.QuotaResourceStorage, "storage"},
			wantCaps: []string{"QUOTA", "QUOTA=RES-STORAGE"},
			wantList: "(STORAGE 1 1024)",
		},
		{
			name:     "lowercase first",
			types:    []imap.QuotaResourceType{"storage", imap.QuotaResourceStorage},
			wantCaps: []string{"QUOTA", "QUOTA=RES-STORAGE"},
			wantList: "(STORAGE 1 1024)",
		},
		{
			name:     "mixed case with another resource",
			types:    []imap.QuotaResourceType{"storage", imap.QuotaResourceStorage, imap.QuotaResourceMessage},
			wantCaps: []string{"QUOTA", "QUOTA=RES-STORAGE", "QUOTA=RES-MESSAGE"},
			wantList: "(STORAGE 1 1024 MESSAGE 1 100)",
		},
		{
			name:     "invalid skipped",
			types:    []imap.QuotaResourceType{"FOO BAR", imap.QuotaResourceMessage, "", imap.QuotaResourceStorage},
			wantCaps: []string{"QUOTA", "QUOTA=RES-MESSAGE", "QUOTA=RES-STORAGE"},
			wantList: "(MESSAGE 1 100 STORAGE 1 1024)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRawQuotaConn(t, func(s quotaTestSession) imapserver.Session {
				return quotaTypesSession{s, tc.types}
			})
			r.cmd("A1", `APPEND INBOX {`+strconv.Itoa(len(simpleRawMessage))+`+}`+"\r\n"+simpleRawMessage)

			if got := quotaCaps(r.capabilities()); strings.Join(got, " ") != strings.Join(tc.wantCaps, " ") {
				t.Errorf("QUOTA capabilities = %q, want %q", got, tc.wantCaps)
			}

			got := r.cmd("A2", `GETQUOTA ""`)
			if tc.wantCaps == nil {
				if len(got) != 1 || !strings.HasPrefix(got[0], "A2 BAD [CLIENTBUG]") {
					t.Errorf("GETQUOTA = %q, want BAD [CLIENTBUG]", got)
				}
				return
			}
			want := []string{`* QUOTA "" ` + tc.wantList, "A2 OK GETQUOTA completed"}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("GETQUOTA = %q, want %q", got, want)
			}
		})
	}
}

// TestQuota_sessionCapabilities checks that QUOTA follows the configured
// capabilities, as the other optional extensions do.
func TestQuota_sessionCapabilities(t *testing.T) {
	r := newRawQuotaConn(t, func(s quotaTestSession) imapserver.Session {
		return quotaCapsSession{s}
	})
	if got := quotaCaps(r.capabilities()); got != nil {
		t.Errorf("QUOTA capabilities = %q, want none", got)
	}
	for i, cmd := range []string{`GETQUOTA ""`, `GETQUOTAROOT INBOX`, `SETQUOTA "" (STORAGE 1)`} {
		tag := "A" + strconv.Itoa(i+1)
		if got := r.cmd(tag, cmd); len(got) != 1 || !strings.HasPrefix(got[0], tag+" BAD [CLIENTBUG]") {
			t.Errorf("%v = %q, want BAD [CLIENTBUG]", cmd, got)
		}
	}
}

// TestQuota_deletedStatus checks the STATUS items RFC 9208 §4.1.4 requires
// with QUOTA=RES-MESSAGE and QUOTA=RES-STORAGE.
func TestQuota_deletedStatus(t *testing.T) {
	r := newRawQuotaConn(t, func(s quotaTestSession) imapserver.Session { return s })
	for i := 0; i < 2; i++ {
		tag := "P" + strconv.Itoa(i)
		if got := r.cmd(tag, `APPEND INBOX {`+strconv.Itoa(len(simpleRawMessage))+`+}`+"\r\n"+simpleRawMessage); !strings.HasPrefix(got[len(got)-1], tag+" OK") {
			t.Fatalf("APPEND = %q", got)
		}
	}

	r.cmd("A1", "SELECT INBOX")
	r.cmd("A2", `STORE 1 +FLAGS (\Deleted)`)
	r.cmd("A3", "UNSELECT")

	want := `* STATUS INBOX (MESSAGES 2 DELETED 1 DELETED-STORAGE ` + strconv.Itoa(len(simpleRawMessage)) + `)`
	if got := r.cmd("A4", "STATUS INBOX (MESSAGES DELETED DELETED-STORAGE)"); len(got) != 2 || got[0] != want {
		t.Errorf("STATUS = %q, want %q", got, want)
	}
}
