package imapclient_test

import (
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// appendExtraMessages appends n messages to INBOX so a test has enough messages
// to build a non-contiguous set.
func appendExtraMessages(t *testing.T, client *imapclient.Client, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		appendCmd := client.Append("INBOX", int64(len(simpleRawMessage)), nil)
		if _, err := appendCmd.Write([]byte(simpleRawMessage)); err != nil {
			t.Fatalf("AppendCommand.Write() = %v", err)
		}
		if err := appendCmd.Close(); err != nil {
			t.Fatalf("AppendCommand.Close() = %v", err)
		}
		if _, err := appendCmd.Wait(); err != nil {
			t.Fatalf("AppendCommand.Wait() = %v", err)
		}
	}
}

// TestStore_UnchangedSinceZero is an end-to-end check of the always-fail probe
// of RFC 7162 §3.1.3.1, exercising every hop: the client must put an explicit
// "UNCHANGEDSINCE 0" on the wire (a zero UnchangedSince alone used to be
// indistinguishable from an absent modifier), the server must parse it as
// present, the backend must fail every message that carries a modification
// sequence, and the resulting MODIFIED response code must parse back into the
// same set.
func TestStore_UnchangedSinceZero(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	// The harness appends one message before selecting; add two more.
	appendExtraMessages(t, client, 2)

	storeFlags := &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}

	numSet := imap.SeqSetNum(1, 3)
	cmd := client.Store(numSet, storeFlags, &imap.StoreOptions{UnchangedSinceSet: true})
	msgs, err := cmd.Collect()
	if err != nil {
		t.Fatalf("Store().Collect() = %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Store().Collect() returned %v messages, want 0: UNCHANGEDSINCE 0 must store nothing", len(msgs))
	}

	modified := cmd.Modified()
	if modified == nil {
		t.Fatal("Modified() = nil, want the messages that failed UNCHANGEDSINCE 0")
	}
	if _, ok := modified.(imap.SeqSet); !ok {
		t.Fatalf("Modified() type = %T, want imap.SeqSet", modified)
	}
	if got, want := modified.String(), numSet.String(); got != want {
		t.Errorf("Modified() = %v, want %v", got, want)
	}

	// Nothing was actually stored.
	fetched, err := client.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatalf("Fetch().Collect() = %v", err)
	}
	for _, msg := range fetched {
		for _, flag := range msg.Flags {
			if flag == imap.FlagSeen {
				t.Errorf("message %v has %v after a failed conditional STORE", msg.SeqNum, imap.FlagSeen)
			}
		}
	}
}

// TestStore_UnchangedSinceAbsent is the counterpart of
// TestStore_UnchangedSinceZero: with the modifier absent the store is
// unconditional, so it must touch every message and report no MODIFIED set.
func TestStore_UnchangedSinceAbsent(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	appendExtraMessages(t, client, 2)

	numSet := imap.SeqSetNum(1, 2, 3)
	// A nil options pointer and a zero options value both mean "no modifier".
	// Each case adds a different flag so the store is always a real change.
	cases := []struct {
		name    string
		options *imap.StoreOptions
		flag    imap.Flag
	}{
		{name: "nil options", options: nil, flag: imap.FlagSeen},
		{name: "zero options", options: &imap.StoreOptions{}, flag: imap.FlagAnswered},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := client.Store(numSet, &imap.StoreFlags{
				Op:    imap.StoreFlagsAdd,
				Flags: []imap.Flag{tc.flag},
			}, tc.options)
			msgs, err := cmd.Collect()
			if err != nil {
				t.Fatalf("Store().Collect() = %v", err)
			}
			if modified := cmd.Modified(); modified != nil {
				t.Errorf("Modified() = %v, want nil for an unconditional STORE", modified)
			}
			if len(msgs) != 3 {
				t.Fatalf("Store().Collect() returned %v messages, want 3", len(msgs))
			}
			for _, msg := range msgs {
				var found bool
				for _, flag := range msg.Flags {
					if flag == tc.flag {
						found = true
					}
				}
				if !found {
					t.Errorf("message %v flags = %v, want %v", msg.SeqNum, msg.Flags, tc.flag)
				}
			}
		})
	}
}
