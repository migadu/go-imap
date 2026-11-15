package imapserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

// qresyncOrderSession returns both kinds of resynchronization data so the wire
// order is checked even when a real mailbox has no expunged or modified messages.
type qresyncOrderSession struct {
	imapserver.SessionIMAP4rev2
	rev2 bool
}

func (s *qresyncOrderSession) Select(_ context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	data := &imap.SelectData{
		Flags:             []imap.Flag{imap.FlagSeen, imap.FlagFlagged},
		PermanentFlags:    []imap.Flag{imap.FlagSeen, imap.FlagFlagged},
		NumMessages:       3,
		NumRecent:         1,
		FirstUnseenSeqNum: 2,
		UIDValidity:       100,
		UIDNext:           5,
		HighestModSeq:     9,
	}
	if s.rev2 {
		data.List = &imap.ListData{Mailbox: mailbox, Delim: '/'}
	}
	if options.QResync != nil {
		data.Vanished = imap.UIDSetNum(2)
		data.Modified = []imap.SelectModifiedData{
			{SeqNum: 1, UID: 1, Flags: []imap.Flag{imap.FlagSeen}, ModSeq: 7},
			{SeqNum: 3, UID: 4, Flags: []imap.Flag{imap.FlagFlagged}, ModSeq: 8},
		}
	}
	return data, nil
}

// RFC 7162 section 3.2.5 opens the mailbox and sends its standard untagged
// responses before processing the resynchronization data. Section 3.2.5.1's
// examples place VANISHED (EARLIER), then the modified-message FETCH responses,
// after those opening responses, including HIGHESTMODSEQ.
func TestSelectQResyncResponseOrder(t *testing.T) {
	for _, rev2 := range []bool{false, true} {
		version := "rev1"
		baseCap := imap.CapIMAP4rev1
		if rev2 {
			version, baseCap = "rev2", imap.CapIMAP4rev2
		}
		for _, command := range []string{"SELECT", "EXAMINE"} {
			for _, qresync := range []bool{false, true} {
				mode := "plain"
				if qresync {
					mode = "QRESYNC"
				}
				t.Run(version+"/"+command+"/"+mode, func(t *testing.T) {
					rc := newSearchRev2ConnWrapped(t, imap.CapSet{baseCap: {}, imap.CapQResync: {}}, func(session imapserver.Session) imapserver.Session {
						return &qresyncOrderSession{SessionIMAP4rev2: session.(imapserver.SessionIMAP4rev2), rev2: rev2}
					})
					rc.do("ENABLE QRESYNC")
					cmd := command + " INBOX"
					if qresync {
						cmd += " (QRESYNC (100 1))"
					}
					response, tagged := rc.try(cmd)
					completion := " OK [READ-WRITE] "
					if command == "EXAMINE" {
						completion = " OK [READ-ONLY] "
					}
					if !strings.Contains(tagged, completion) {
						t.Fatalf("%s: want %s completion, got:\n%s", cmd, strings.TrimSpace(completion), response)
					}

					standard := []string{
						"* 3 EXISTS\r\n",
						"* OK [UIDVALIDITY 100] ",
						"* OK [UIDNEXT 5] ",
						"* FLAGS ",
						"* OK [PERMANENTFLAGS ",
						"* OK [HIGHESTMODSEQ 9] ",
					}
					if rev2 {
						standard = append(standard, "* LIST ")
						if strings.Contains(response, " RECENT\r\n") || strings.Contains(response, "[UNSEEN ") {
							t.Errorf("IMAP4rev2 sent an obsolete opening response:\n%s", response)
						}
					} else {
						standard = append(standard, "* 1 RECENT\r\n", "* OK [UNSEEN 2] ")
					}
					lastStandard := -1
					for _, prefix := range standard {
						index := strings.Index(response, prefix)
						if index < 0 {
							t.Fatalf("missing opening response %q:\n%s", prefix, response)
						}
						if index > lastStandard {
							lastStandard = index
						}
					}

					vanished := strings.Index(response, "* VANISHED (EARLIER) 2\r\n")
					firstFetch := strings.Index(response, "* 1 FETCH (UID 1 ")
					lastFetch := strings.Index(response, "* 3 FETCH (UID 4 ")
					if !qresync {
						if strings.Contains(response, "* VANISHED ") || strings.Contains(response, " FETCH (") {
							t.Fatalf("plain %s sent resynchronization data:\n%s", command, response)
						}
						return
					}
					if vanished < 0 || firstFetch < 0 || lastFetch < 0 {
						t.Fatalf("missing VANISHED or modified-message FETCH response:\n%s", response)
					}
					if vanished <= lastStandard || firstFetch <= lastStandard || lastFetch <= lastStandard {
						t.Errorf("resynchronization data preceded the opening responses:\n%s", response)
					}
					if vanished >= firstFetch || firstFetch >= lastFetch || lastFetch >= strings.Index(response, tagged) {
						t.Errorf("want VANISHED, modified FETCH responses, then tagged completion:\n%s", response)
					}
				})
			}
		}
	}
}
