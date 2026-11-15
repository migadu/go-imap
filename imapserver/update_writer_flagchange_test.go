package imapserver

import (
	"bufio"
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

// A backend cannot treat WriteMessageFlags returning nil as proof of delivery:
// NOTIFY can suppress the response. The public filter query must follow the
// same live filter as the writer, including restoration after NOTIFY ends.
func TestUpdateWriterFlagChangeAllowedMatchesDelivery(t *testing.T) {
	local, peer := net.Pipe()
	t.Cleanup(func() { local.Close(); peer.Close() })
	var out bytes.Buffer
	conn := &Conn{
		conn:   local,
		bw:     bufio.NewWriter(&out),
		state:  imap.ConnStateSelected,
		server: New(&Options{Caps: imap.CapSet{imap.CapIMAP4rev1: {}}}),
	}
	writer := &UpdateWriter{conn: conn}
	set := func(spec imap.NotifyMailboxSpec, events ...imap.NotifyEvent) func() {
		return func() {
			conn.setNotifySelectedEvents(&imap.NotifyOptions{Items: []imap.NotifyItem{{
				MailboxSpec: spec,
				Events:      events,
			}}}, nil)
		}
	}
	for _, step := range []struct {
		name  string
		apply func()
		want  bool
	}{
		{"before NOTIFY", nil, true},
		{"FlagChange omitted", set(imap.NotifyMailboxSpecSelected, imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge), false},
		{"FlagChange restored", set(imap.NotifyMailboxSpecSelected, imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge, imap.NotifyEventFlagChange), true},
		{"NOTIFY NONE", func() { conn.setNotifySelectedEvents(nil, nil) }, false},
		{"SELECTED omitted", set(imap.NotifyMailboxSpecPersonal, imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge, imap.NotifyEventFlagChange), false},
		{"SELECTED-DELAYED FlagChange", set(imap.NotifyMailboxSpecSelectedDelayed, imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge, imap.NotifyEventFlagChange), true},
		{"NOTIFY reset", conn.resetNotifySelectedEvents, true},
	} {
		t.Run(step.name, func(t *testing.T) {
			if step.apply != nil {
				step.apply()
			}
			if got := writer.FlagChangeAllowed(); got != step.want {
				t.Fatalf("FlagChangeAllowed = %v, want %v", got, step.want)
			}
			out.Reset()
			if err := writer.WriteMessageFlags(1, 7, []imap.Flag{imap.FlagSeen}, 0); err != nil {
				t.Fatalf("WriteMessageFlags: %v", err)
			}
			if step.want {
				if got := out.String(); !strings.HasPrefix(got, "* 1 FETCH (") || !strings.Contains(got, "UID 7") || !strings.Contains(got, `FLAGS (\Seen)`) || !strings.HasSuffix(got, ")\r\n") {
					t.Fatalf("allowed flag change was not delivered: %q", got)
				}
			} else if out.Len() != 0 {
				t.Fatalf("filtered flag change was delivered: %q", out.String())
			}
		})
	}
}
