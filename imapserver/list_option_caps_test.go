package imapserver_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type listOptionSession struct {
	imapserver.Session
	extra         []imap.Cap
	listCalls     atomic.Int32
	metadataCalls atomic.Int32
}

func (s *listOptionSession) AdditionalCapabilities() []imap.Cap { return s.extra }

func (s *listOptionSession) List(ctx context.Context, w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	s.listCalls.Add(1)
	return s.Session.List(ctx, w, ref, patterns, options)
}

func (s *listOptionSession) GetMetadata(_ context.Context, mailbox string, _ []string, _ *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	s.metadataCalls.Add(1)
	return &imap.GetMetadataData{Mailbox: mailbox, Entries: map[string]*[]byte{}}, nil
}

func (s *listOptionSession) SetMetadata(context.Context, string, map[string]*[]byte) error {
	return nil
}

type listOptionCapsSession struct {
	*listOptionSession
	caps imap.CapSet
}

func (s *listOptionCapsSession) GetCapabilities() imap.CapSet { return s.caps }

// Configurations that advertise an individual LIST option must continue to
// accept that option without granting unrelated LIST-EXTENDED functionality.
// In particular, RFC 6154 section 5.2 shows SPECIAL-USE advertised on its own.
func TestListAdvertisedOptionsWithoutListExtended(t *testing.T) {
	generic := []string{
		`LIST () "" "*"`,
		`LIST "" "*" RETURN ()`,
		`LIST "" ("*")`,
		`LIST "" ("INBOX" "Archive")`,
		`LIST (SUBSCRIBED) "" "*"`,
		`LIST (REMOTE) "" "*"`,
		`LIST (SUBSCRIBED RECURSIVEMATCH) "" "*"`,
		`LIST "" "*" RETURN (SUBSCRIBED)`,
		`LIST "" "*" RETURN (CHILDREN)`,
	}
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		session imap.CapSet
		extra   []imap.Cap
		served  []string
		refused []string
	}{
		{
			name: "SPECIAL-USE",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}},
			served: []string{
				`LIST (SPECIAL-USE) "" "*"`,
				`LIST "" "*" RETURN (SPECIAL-USE)`,
			},
			refused: []string{
				`LIST "" "*" RETURN (STATUS (MESSAGES))`,
				`LIST "" "*" RETURN (METADATA)`,
				`LIST "" "*" RETURN (SPECIAL-USE STATUS (MESSAGES))`,
				`LIST "" "*" RETURN (SPECIAL-USE CHILDREN)`,
				`LIST (SUBSCRIBED SPECIAL-USE) "" "*"`,
				`LIST (SPECIAL-USE) "" ("*")`,
			},
		},
		{
			name: "LIST-STATUS",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
			served: []string{
				`LIST "" "*" RETURN (STATUS (MESSAGES))`,
			},
			refused: []string{
				`LIST (SPECIAL-USE) "" "*"`,
				`LIST "" "*" RETURN (SPECIAL-USE)`,
				`LIST "" "*" RETURN (METADATA)`,
				`LIST "" "*" RETURN (STATUS (MESSAGES) SPECIAL-USE)`,
				`LIST "" "*" RETURN (STATUS (MESSAGES) CHILDREN)`,
				`LIST "" ("*") RETURN (STATUS (MESSAGES))`,
			},
		},
		{
			name: "LIST-METADATA",
			caps: imap.CapSet{imap.CapIMAP4rev1: {}},
			// The existing additional-capability hook can advertise LIST-METADATA.
			extra: []imap.Cap{imap.CapListMetadata},
			served: []string{
				`LIST "" "*" RETURN (METADATA)`,
				`LIST "" "*" RETURN (METADATA ())`,
				`LIST "" "*" RETURN (METADATA (/private/comment))`,
			},
			refused: []string{
				`LIST (SPECIAL-USE) "" "*"`,
				`LIST "" "*" RETURN (SPECIAL-USE)`,
				`LIST "" "*" RETURN (STATUS (MESSAGES))`,
				`LIST "" "*" RETURN (STATUS (MESSAGES) METADATA)`,
				`LIST "" "*" RETURN (CHILDREN METADATA)`,
				`LIST "" ("*") RETURN (METADATA)`,
			},
		},
		{
			name:  "combined options",
			caps:  imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}, imap.CapListStatus: {}},
			extra: []imap.Cap{imap.CapListMetadata},
			served: []string{
				`LIST (SPECIAL-USE) "" "*" RETURN (STATUS (MESSAGES) METADATA)`,
				`LIST "" "*" RETURN (SPECIAL-USE STATUS (MESSAGES) METADATA ())`,
				`LIST () "" "*" RETURN (STATUS (MESSAGES))`,
				`LIST (SPECIAL-USE) "" "*" RETURN ()`,
			},
			refused: []string{
				`LIST (SPECIAL-USE REMOTE) "" "*"`,
				`LIST "" "*" RETURN (SPECIAL-USE STATUS (MESSAGES) CHILDREN)`,
				`LIST (SPECIAL-USE) "" ("INBOX" "Archive") RETURN (STATUS (MESSAGES))`,
			},
		},
		{
			name:    "session filters other options out",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListExtended: {}, imap.CapSpecialUse: {}, imap.CapListStatus: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapSpecialUse: {}},
			served:  []string{`LIST (SPECIAL-USE) "" "*"`},
			refused: []string{`LIST "" "*" RETURN (STATUS (MESSAGES))`},
		},
		{
			name:    "session supplies LIST-STATUS",
			caps:    imap.CapSet{imap.CapIMAP4rev1: {}},
			session: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}},
			served:  []string{`LIST "" "*" RETURN (STATUS (MESSAGES))`},
			refused: []string{`LIST "" "*" RETURN (SPECIAL-USE)`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probe *listOptionSession
			rc := newSearchRev2ConnWrapped(t, tc.caps, func(sess imapserver.Session) imapserver.Session {
				probe = &listOptionSession{Session: sess, extra: tc.extra}
				if tc.session != nil {
					return &listOptionCapsSession{listOptionSession: probe, caps: tc.session}
				}
				return probe
			})
			caps := rc.do("CAPABILITY")
			if strings.Contains(caps, "LIST-EXTENDED") {
				t.Fatalf("unexpected LIST-EXTENDED advertisement: %q", caps)
			}
			effective := tc.caps
			if tc.session != nil {
				effective = tc.session
			}
			for _, cap := range []imap.Cap{imap.CapSpecialUse, imap.CapListStatus, imap.CapListMetadata} {
				want := effective.Has(cap)
				for _, extra := range tc.extra {
					want = want || extra == cap
				}
				got := strings.Contains(caps, " "+string(cap)+" ") || strings.Contains(caps, " "+string(cap)+"\r\n")
				if got != want {
					t.Fatalf("CAPABILITY includes %s = %v, want %v: %q", cap, got, want, caps)
				}
			}
			for _, cmd := range tc.served {
				rc.do(cmd)
			}
			for _, cmd := range append(generic, tc.refused...) {
				listBefore, metadataBefore := probe.listCalls.Load(), probe.metadataCalls.Load()
				resp, tagged := rc.try(cmd)
				if !strings.Contains(tagged, " BAD [CLIENTBUG] ") {
					t.Errorf("%s: want BAD [CLIENTBUG], got:\n%s", cmd, resp)
				}
				if probe.listCalls.Load() != listBefore || probe.metadataCalls.Load() != metadataBefore {
					t.Errorf("refused command reached backend: %s", cmd)
				}
				rc.do("NOOP")
			}
			rc.do(`LIST "" "*"`)
			rc.do(`LSUB "" "*"`)
		})
	}
}
