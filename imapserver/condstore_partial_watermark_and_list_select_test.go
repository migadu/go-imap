package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type auditPartialModSeqSession struct {
	imapserver.Session
	conn            *imapserver.Conn
	savedNotifyW    *imapserver.UpdateWriter
	failSetNotify   error
	failPollOnCmd   atomic.Bool
	emitModSeqPoll  bool
	failFetch       error
	failSearch      error
	emitInSearch    bool
	failSort        error
	failThread      error
	failStore       error
	failStatus      error
	failList        error
	failUnselect    error
	emitInUnselect  bool
	sessionClosedCh chan struct{}
}

func (s *auditPartialModSeqSession) Close() error {
	err := s.Session.Close()
	if s.sessionClosedCh != nil {
		close(s.sessionClosedCh)
	}
	return err
}

func (s *auditPartialModSeqSession) HighestModSeq() uint64 { return 3 }

func (s *auditPartialModSeqSession) Select(ctx context.Context, box string, opts *imap.SelectOptions) (*imap.SelectData, error) {
	d, err := s.Session.Select(ctx, box, opts)
	if err == nil {
		d.HighestModSeq = 3
	}
	return d, err
}

func (s *auditPartialModSeqSession) Unselect(ctx context.Context) error {
	if s.failUnselect != nil {
		if s.emitInUnselect && s.savedNotifyW != nil {
			if err := s.savedNotifyW.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
				return err
			}
		}
		return s.failUnselect
	}
	return s.Session.Unselect(ctx)
}

func (s *auditPartialModSeqSession) SetNotify(ctx context.Context, opts *imap.NotifyOptions, w *imapserver.UpdateWriter) error {
	s.savedNotifyW = w
	if s.failSetNotify != nil {
		if err := w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
			return err
		}
		return s.failSetNotify
	}
	if sn, ok := s.Session.(imapserver.SessionNotify); ok {
		return sn.SetNotify(ctx, opts, w)
	}
	return nil
}

func (s *auditPartialModSeqSession) NotifyPoll(ctx context.Context, w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	select {
	case <-ctx.Done():
	case <-stop:
	}
	return nil
}

func (s *auditPartialModSeqSession) Poll(ctx context.Context, w *imapserver.UpdateWriter, allowExpunge bool) error {
	if s.failPollOnCmd.Load() {
		if s.emitModSeqPoll {
			if err := w.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
				return err
			}
		}
		return &imap.Error{
			Type: imap.StatusResponseTypeBye,
			Text: "Backend lost mailbox during poll",
		}
	}
	return s.Session.Poll(ctx, w, allowExpunge)
}

func (s *auditPartialModSeqSession) Fetch(ctx context.Context, w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	if s.failFetch != nil {
		mw := w.CreateMessage(1)
		mw.WriteUID(1)
		mw.WriteModSeq(10)
		if err := mw.Close(); err != nil {
			return err
		}
		return s.failFetch
	}
	return s.Session.Fetch(ctx, w, numSet, options)
}

func (s *auditPartialModSeqSession) Search(ctx context.Context, kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	if s.failSearch != nil {
		if s.emitInSearch && s.savedNotifyW != nil {
			if err := s.savedNotifyW.WriteMessageFlags(1, 1, []imap.Flag{imap.FlagSeen}, 10); err != nil {
				return nil, err
			}
		}
		return nil, s.failSearch
	}
	return s.Session.Search(ctx, kind, criteria, options)
}

func (s *auditPartialModSeqSession) Sort(ctx context.Context, kind imapserver.NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	if s.failSort != nil {
		return nil, s.failSort
	}
	return s.Session.Sort(ctx, kind, sortCriteria, charset, searchCriteria, options)
}

func (s *auditPartialModSeqSession) Thread(ctx context.Context, numKind imapserver.NumKind, algorithm imap.ThreadAlgorithm, charset string, criteria *imap.SearchCriteria) ([]imap.ThreadData, error) {
	if s.failThread != nil {
		return nil, s.failThread
	}
	return s.Session.(imapserver.SessionThread).Thread(ctx, numKind, algorithm, charset, criteria)
}

func (s *auditPartialModSeqSession) Store(ctx context.Context, w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	if s.failStore != nil {
		return s.failStore
	}
	return s.Session.Store(ctx, w, numSet, flags, options)
}

func (s *auditPartialModSeqSession) Status(ctx context.Context, mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	if s.failStatus != nil {
		return nil, s.failStatus
	}
	return s.Session.Status(ctx, mailbox, options)
}

func (s *auditPartialModSeqSession) List(ctx context.Context, w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if s.failList != nil {
		return s.failList
	}
	return s.Session.List(ctx, w, ref, patterns, options)
}

func newAuditPartialModSeqConn(t *testing.T, configure func(*auditPartialModSeqSession)) (*storeTestConn, *auditPartialModSeqSession) {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser("user", "pass")
	for _, box := range []string{"INBOX", "Other"} {
		if err := u.Create(context.Background(), box, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(u)

	sessions := make(chan *auditPartialModSeqSession, 1)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			sess := &auditPartialModSeqSession{
				Session:         mem.NewSession(),
				conn:            conn,
				sessionClosedCh: make(chan struct{}),
			}
			if configure != nil {
				configure(sess)
			}
			sessions <- sess
			return sess, nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:             {},
			imap.CapCondStore:             {},
			imap.CapNotify:                {},
			imap.CapEnable:                {},
			imap.CapListStatus:            {},
			imap.CapSort:                  {},
			imap.Cap("THREAD=REFERENCES"): {},
		},
		InsecureAuth: true,
		Logger:       log.New(io.Discard, "", 0),
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
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	c := &storeTestConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	if _, err := c.br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	sess := <-sessions
	c.do("l", "LOGIN user pass")
	c.appendMessage("body")
	return c, sess
}

func TestPartialModSeqBeforeBadPreservesWatermarkAndRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preNotify bool
		tag       string
		cmd       string
		cfg       func(*auditPartialModSeqSession)
	}{
		{
			name: "NOTIFY_SetNotify_partial_MODSEQ_then_BAD",
			tag:  "n",
			cmd:  "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge FlagChange))",
			cfg: func(s *auditPartialModSeqSession) {
				s.failSetNotify = fmt.Errorf("backend wrapper: %w", &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Text: "Backend rejected NOTIFY after partial update",
				})
			},
		},
		{
			name: "FETCH_partial_MODSEQ_then_BAD",
			tag:  "f",
			cmd:  "FETCH 1 (UID MODSEQ)",
			cfg: func(s *auditPartialModSeqSession) {
				s.failFetch = fmt.Errorf("backend wrapper: %w", &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Text: "Backend rejected FETCH after partial item",
				})
			},
		},
		{
			name:      "SEARCH_MODSEQ_concurrent_NOTIFY_MODSEQ_then_BAD",
			preNotify: true,
			tag:       "sr",
			cmd:       "SEARCH MODSEQ 1",
			cfg: func(s *auditPartialModSeqSession) {
				s.emitInSearch = true
				s.failSearch = fmt.Errorf("backend wrapper: %w", &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Text: "Backend rejected SEARCH after concurrent update",
				})
			},
		},
		{
			name:      "SELECT_CONDSTORE_Unselect_partial_MODSEQ_then_BAD",
			preNotify: true,
			tag:       "sel",
			cmd:       "SELECT Other (CONDSTORE)",
			cfg: func(s *auditPartialModSeqSession) {
				s.emitInUnselect = true
				s.failUnselect = fmt.Errorf("backend wrapper: %w", &imap.Error{
					Type: imap.StatusResponseTypeBad,
					Text: "Backend rejected Unselect after partial update",
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, alreadyEnabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("already_enabled=%v", alreadyEnabled), func(t *testing.T) {
					c, sess := newAuditPartialModSeqConn(t, nil)
					if alreadyEnabled {
						c.do("e", "ENABLE CONDSTORE")
					}
					c.do("s", "SELECT INBOX")
					if tc.preNotify {
						c.do("n0", "NOTIFY SET (SELECTED (MessageNew (UID FLAGS) MessageExpunge FlagChange))")
					}
					tc.cfg(sess)
					if got := sess.conn.CondStoreEnabled(); got != alreadyEnabled {
						t.Fatalf("fixture CONDSTORE = %v, want %v", got, alreadyEnabled)
					}

					resp := c.do(tc.tag, tc.cmd)
					badIdx := strings.Index(resp, tc.tag+" BAD")
					if badIdx < 0 {
						t.Fatalf("missing %s BAD: %q", tc.tag, resp)
					}
					modSeqIdx := strings.Index(resp[:badIdx], "MODSEQ (10)")
					if modSeqIdx < 0 {
						t.Fatalf("fixture did not write MODSEQ (10) before BAD: %q", resp)
					}
					watermarkIdx := strings.Index(resp[:badIdx], "* OK [HIGHESTMODSEQ 3]")
					if watermarkIdx < modSeqIdx {
						t.Errorf("lost compensating * OK [HIGHESTMODSEQ 3] before %s BAD after partial MODSEQ (10) FETCH: %q", tc.tag, resp)
					}
					if got := sess.conn.CondStoreEnabled(); got != alreadyEnabled {
						t.Errorf("CONDSTORE after BAD = %v, want pre-command state %v", got, alreadyEnabled)
					}

					storeResp := c.do("st", `STORE 1 +FLAGS (\Flagged)`)
					if gotModSeq := strings.Contains(storeResp, "MODSEQ ("); gotModSeq != alreadyEnabled {
						t.Errorf("subsequent STORE MODSEQ = %v, want %v: %q", gotModSeq, alreadyEnabled, storeResp)
					}
				})
			}
		})
	}
}

func TestEnablingCommandsByeFlushesWatermarkAndRollsBackCondStore(t *testing.T) {
	byeErr := &imap.Error{
		Type: imap.StatusResponseTypeBye,
		Text: "Backend terminated session",
	}

	for _, tc := range []struct {
		name            string
		alreadyEnabled  bool
		preNotify       bool
		cmd             string
		wantModSeq10    bool
		wantCondStore   bool
		cfgBeforeTarget func(*auditPartialModSeqSession)
	}{
		{
			name:          "NOTIFY_Poll_BYE_after_partial_MODSEQ",
			cmd:           "NOTIFY SET (SELECTED (MessageNew (UID MODSEQ) MessageExpunge FlagChange))",
			wantModSeq10:  true,
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.emitModSeqPoll = true
				s.failPollOnCmd.Store(true)
			},
		},
		{
			name:          "FETCH_MODSEQ_BYE_after_partial_MODSEQ",
			cmd:           "FETCH 1 (UID MODSEQ)",
			wantModSeq10:  true,
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failFetch = byeErr
			},
		},
		{
			name:          "SEARCH_MODSEQ_BYE",
			cmd:           "SEARCH MODSEQ 1",
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failSearch = byeErr
			},
		},
		{
			name:          "SORT_MODSEQ_BYE",
			cmd:           "SORT (ARRIVAL) UTF-8 MODSEQ 1",
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failSort = byeErr
			},
		},
		{
			name:          "THREAD_MODSEQ_BYE",
			cmd:           "THREAD REFERENCES UTF-8 MODSEQ 1",
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failThread = byeErr
			},
		},
		{
			name:          "STORE_UNCHANGEDSINCE_BYE",
			cmd:           `STORE 1 (UNCHANGEDSINCE 1) +FLAGS (\Seen)`,
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failStore = byeErr
			},
		},
		{
			name:          "STATUS_HIGHESTMODSEQ_BYE",
			cmd:           "STATUS Other (HIGHESTMODSEQ)",
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failStatus = byeErr
			},
		},
		{
			name:          "LIST_RETURN_STATUS_HIGHESTMODSEQ_BYE",
			cmd:           `LIST "" "Other" RETURN (STATUS (HIGHESTMODSEQ))`,
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failList = byeErr
			},
		},
		{
			name:          "SELECT_CONDSTORE_Unselect_partial_MODSEQ_then_BYE",
			preNotify:     true,
			cmd:           "SELECT Other (CONDSTORE)",
			wantModSeq10:  true,
			wantCondStore: false,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.emitInUnselect = true
				s.failUnselect = byeErr
			},
		},
		{
			name:          "ENABLE_CONDSTORE_already_sent_ENABLED_before_Poll_BYE",
			cmd:           "ENABLE CONDSTORE",
			wantCondStore: true,
			cfgBeforeTarget: func(s *auditPartialModSeqSession) {
				s.failPollOnCmd.Store(true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sess := newAuditPartialModSeqConn(t, nil)
			if tc.alreadyEnabled {
				c.do("e", "ENABLE CONDSTORE")
			}
			c.do("s", "SELECT INBOX")
			if tc.preNotify {
				c.do("n0", "NOTIFY SET (SELECTED (MessageNew (UID FLAGS) MessageExpunge FlagChange))")
			}
			tc.cfgBeforeTarget(sess)

			if _, err := fmt.Fprintf(c.conn, "b %s\r\n", tc.cmd); err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(c.br)
			if err != nil {
				t.Fatal(err)
			}
			_ = c.conn.Close()
			<-sess.sessionClosedCh

			resp := string(raw)
			byeIdx := strings.Index(resp, "* BYE")
			if byeIdx < 0 {
				t.Fatalf("missing * BYE: %q", resp)
			}
			if tc.wantModSeq10 {
				modIdx := strings.Index(resp[:byeIdx], "MODSEQ (10)")
				if modIdx < 0 {
					t.Fatalf("expected MODSEQ (10) before BYE, got %q", resp)
				}
				wmIdx := strings.Index(resp[:byeIdx], "* OK [HIGHESTMODSEQ 3]")
				if wmIdx < modIdx {
					t.Errorf("lost compensating * OK [HIGHESTMODSEQ 3] before BYE after partial MODSEQ (10): %q", resp)
				}
			}
			if got := sess.conn.CondStoreEnabled(); got != tc.wantCondStore {
				t.Errorf("CONDSTORE after BYE = %v, want %v (wire=%q)", got, tc.wantCondStore, resp)
			}
		})
	}
}

func TestListNonEmptySelectOptsWithoutSpecialUseOrListExtendedAnswersBadBeforeTooBig(t *testing.T) {
	t.Run("synchronizing oversized select-opt literal without SPECIAL-USE or LIST-EXTENDED", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			caps imap.CapSet
			want string
		}{
			{
				name: "LIST-STATUS only rejects non-empty select-opts with BAD",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
				want: " BAD [CLIENTBUG] ",
			},
			{
				name: "LIST-METADATA only rejects non-empty select-opts with BAD",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMetadata: {}, imap.CapListMetadata: {}},
				want: " BAD [CLIENTBUG] ",
			},
			{
				name: "SPECIAL-USE permits non-empty select-opts and answers TOOBIG",
				caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}},
				want: " NO [TOOBIG] ",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var probe *listOptionSession
				rc := newSearchRev2ConnWrapped(t, tc.caps, func(sess imapserver.Session) imapserver.Session {
					probe = &listOptionSession{Session: sess}
					return probe
				})
				cmd := `LIST ({4097}`
				tagged, contReqs := rc.exchange(cmd)
				if !strings.Contains(tagged, tc.want) || contReqs != 0 {
					t.Fatalf("%s = %q (cont=%d), want %q without continuation", cmd, strings.TrimRight(tagged, "\r\n"), contReqs, tc.want)
				}
				if probe.listCalls.Load() != 0 {
					t.Fatalf("rejected LIST reached backend")
				}
				rc.requireInSync(cmd)
			})
		}
	})

	t.Run("LITERAL+ oversized select-opt literal on LIST-STATUS server answers BAD and stays in sync", func(t *testing.T) {
		var probe *listOptionSession
		caps := imap.CapSet{
			imap.CapIMAP4rev1:   {},
			imap.CapLiteralPlus: {},
			imap.CapListStatus:  {},
		}
		rc := dialSearchRev2Conn(t, caps, func(sess imapserver.Session) imapserver.Session {
			probe = &listOptionSession{Session: sess}
			return probe
		})
		rc.do("LOGIN " + searchRev2Username + " " + searchRev2Password)

		opt4097 := strings.Repeat("S", 4097)
		raw := fmt.Sprintf("L1 LIST ({%d+}\r\n%s) \"\" \"*\" RETURN (STATUS (MESSAGES))\r\nL2 NOOP\r\n", len(opt4097), opt4097)
		if _, err := fmt.Fprint(rc.c, raw); err != nil {
			t.Fatalf("write L1: %v", err)
		}
		_ = rc.c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line1, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L1: %v", err)
		}
		line2, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read L2: %v", err)
		}
		if !strings.HasPrefix(line2, "L2 OK ") {
			t.Fatalf("stream out of sync on L2: %q", strings.TrimRight(line2, "\r\n"))
		}
		if !strings.HasPrefix(line1, "L1 BAD [CLIENTBUG] ") {
			t.Fatalf("want L1 BAD [CLIENTBUG], got %q", strings.TrimRight(line1, "\r\n"))
		}
		if probe.listCalls.Load() != 0 {
			t.Fatalf("rejected LIST reached backend")
		}
	})
}
