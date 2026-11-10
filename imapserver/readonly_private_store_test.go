package imapserver

import (
	"context"
	"errors"
	"testing"

	"github.com/emersion/go-imap/v2"
)

type privateStoreSession struct {
	unselectRecordingSession
	stores   int
	storeErr error
}

func (s *privateStoreSession) Store(context.Context, *FetchWriter, imap.NumSet, *imap.StoreFlags, *imap.StoreOptions) error {
	s.stores++
	return s.storeErr
}

func TestReadOnlySelectPrivateStore(t *testing.T) {
	for _, kind := range []NumKind{NumKindSeq, NumKindUID} {
		for _, tc := range []struct {
			name             string
			examine, private bool
			allow            bool
		}{
			{"SELECT with private flags", false, true, true},
			{"SELECT without private flags", false, false, false},
			{"EXAMINE ignores private flags", true, true, false},
		} {
			t.Run(tc.name+"/"+kind.String(), func(t *testing.T) {
				session := &privateStoreSession{unselectRecordingSession: unselectRecordingSession{
					selectData: &imap.SelectData{ReadOnly: true},
				}}
				if tc.private {
					session.selectData.PermanentFlags = []imap.Flag{imap.FlagSeen}
				}
				conn := newUnselectTestConn(t, session)
				if err := conn.handleSelect("T1", argsDecoder(" INBOX"), tc.examine); err != nil {
					t.Fatal(err)
				}
				for _, item := range []string{"+FLAGS", "-FLAGS.SILENT"} {
					err := conn.handleStore(argsDecoder(" 1 "+item+` (\Seen)`), kind)
					if (err == nil) != tc.allow {
						t.Errorf("%s error=%v; allow=%v", item, err, tc.allow)
					}
				}
				want := 0
				if tc.allow {
					want = 2
				}
				if session.stores != want {
					t.Errorf("backend STORE calls=%d; want %d", session.stores, want)
				}
				// READ-ONLY must still protect EXPUNGE and suppress CLOSE's
				// implicit expunge, including when private STORE is permitted.
				if err := conn.checkWritableMailbox(); err == nil {
					t.Error("private STORE enabled shared mailbox mutation")
				}
				if err := conn.handleUnselect(crlfDecoder(), true); err != nil {
					t.Fatal(err)
				}
				if session.expungeCalled {
					t.Error("CLOSE expunged a read-only selection")
				}
			})
		}
	}
}

func TestReadOnlySelectPrivateStoreResetsAndPreservesBackendRefusal(t *testing.T) {
	refusal := &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNoPerm, Text: "shared flags require w"}
	session := &privateStoreSession{unselectRecordingSession: unselectRecordingSession{
		selectData: &imap.SelectData{ReadOnly: true, PermanentFlags: []imap.Flag{imap.FlagSeen}},
	}, storeErr: refusal}
	conn := newUnselectTestConn(t, session)
	for _, examine := range []bool{false, true, false} {
		if err := conn.handleSelect("T1", argsDecoder(" INBOX"), examine); err != nil {
			t.Fatal(err)
		}
		before := session.stores
		err := conn.handleStore(argsDecoder(` 1 FLAGS (\Seen)`), NumKindSeq)
		if examine {
			if err == nil || session.stores != before {
				t.Fatal("EXAMINE delegated STORE")
			}
		} else if !errors.Is(err, refusal) || session.stores != before+1 {
			t.Fatalf("backend refusal lost: %v; calls=%d", err, session.stores-before)
		}
	}
	session.selectData.PermanentFlags = nil
	if err := conn.handleSelect("T2", argsDecoder(" INBOX"), false); err != nil {
		t.Fatal(err)
	}
	before := session.stores
	if err := conn.handleStore(argsDecoder(` 1 +FLAGS (\Seen)`), NumKindSeq); err == nil || session.stores != before {
		t.Fatal("private STORE permission survived selection without permanent flags")
	}
}
