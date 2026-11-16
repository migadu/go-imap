package imapserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type listMetadataEntryRecorder struct {
	*listOptionSession
	entries chan []string
}

func (s *listMetadataEntryRecorder) GetMetadata(ctx context.Context, mailbox string, entries []string, options *imap.GetMetadataOptions) (*imap.GetMetadataData, error) {
	s.entries <- append([]string(nil), entries...)
	return s.listOptionSession.GetMetadata(ctx, mailbox, entries, options)
}

func TestListRepeatedMetadataValidatesEveryEntry(t *testing.T) {
	for _, tc := range []struct {
		name, options string
		valid         bool
	}{
		{
			name:    "invalid quoted entry before valid option",
			options: `METADATA (/private/) METADATA (/private/comment)`,
		},
		{
			name:    "invalid entry before bare option",
			options: `METADATA (/private/) METADATA`,
		},
		{
			name:    "invalid literal entry before valid option",
			options: "METADATA ({9+}\r\n/private/) METADATA (/private/comment)",
		},
		{
			name:    "invalid entry before literal in later option",
			options: "METADATA (/private/) METADATA ({16+}\r\n/private/comment)",
		},
		{
			name:    "invalid final option",
			options: `METADATA (/private/comment) METADATA (/private/)`,
		},
		{
			name:    "valid repeated option",
			options: `METADATA (/private/comment) METADATA (/private/comment)`,
			valid:   true,
		},
		{
			name:    "valid repeated option with literal",
			options: "METADATA (/private/comment) METADATA ({16+}\r\n/private/comment)",
			valid:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &listMetadataEntryRecorder{
				listOptionSession: &listOptionSession{},
				entries:           make(chan []string, 8),
			}
			rc := newSearchRev2ConnWrapped(t, imap.CapSet{
				imap.CapIMAP4rev1:    {},
				imap.CapMetadata:     {},
				imap.CapListExtended: {},
				imap.CapListMetadata: {},
			}, func(sess imapserver.Session) imapserver.Session {
				probe.Session = sess
				return probe
			})

			resp, tagged := rc.try(`LIST "" "INBOX" RETURN (` + tc.options + `)`)
			wantCalls := int32(0)
			if tc.valid {
				wantCalls = 1
				if !strings.Contains(tagged, " OK ") {
					t.Errorf("valid repeated option was refused: %q", resp)
				}
			} else if !strings.Contains(tagged, " BAD ") || resp != tagged {
				t.Errorf("invalid entry requires only a tagged BAD: %q", resp)
			}
			if probe.listCalls.Load() != wantCalls || probe.metadataCalls.Load() != wantCalls {
				t.Errorf("backend calls: LIST=%d METADATA=%d, want %d each", probe.listCalls.Load(), probe.metadataCalls.Load(), wantCalls)
			}
			if tc.valid && probe.metadataCalls.Load() == 1 {
				entries := <-probe.entries
				if len(entries) != 1 || entries[0] != "/private/comment" {
					t.Errorf("repeated option produced duplicate or wrong entries: %q", entries)
				}
			}
			if reply := rc.do("NOOP"); strings.Count(reply, "\r\n") != 1 {
				t.Errorf("command stream was not clean after LIST: %q", reply)
			}
		})
	}
}
