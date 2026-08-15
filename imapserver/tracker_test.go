package imapserver_test

import (
	"testing"

	"github.com/emersion/go-imap/v2/imapserver"
)

type trackerUpdate struct {
	expunge     uint32
	numMessages uint32
}

var sessionTrackerSeqNumTests = []struct {
	name                       string
	pending                    []trackerUpdate
	clientSeqNum, serverSeqNum uint32
}{
	{
		// Bug: QueueNumMessages increments by more than 1.
		// EncodeSeqNum must return 0 for ALL new seqNums (prevNumMessages+1 ..
		// numMessages), not just the last one.
		// Mailbox starts at 42; 3 messages appended → numMessages=45.
		// New messages are at server seqNums 43, 44, 45 — all must map to 0.
		name:         "append_multi_first_new",
		pending:      []trackerUpdate{{numMessages: 45}},
		clientSeqNum: 0,
		serverSeqNum: 43, // first new message — was returning 43 before the fix
	},
	{
		name:         "append_multi_middle_new",
		pending:      []trackerUpdate{{numMessages: 45}},
		clientSeqNum: 0,
		serverSeqNum: 44, // middle new message
	},
	{
		name:         "append_multi_last_new",
		pending:      []trackerUpdate{{numMessages: 45}},
		clientSeqNum: 0,
		serverSeqNum: 45, // last new message — was already handled correctly
	},
	{
		name:         "append_multi_old_msg",
		pending:      []trackerUpdate{{numMessages: 45}},
		clientSeqNum: 42,
		serverSeqNum: 42, // pre-existing message — must not be affected
	},
	{
		name:         "noop",
		pending:      nil,
		clientSeqNum: 20,
		serverSeqNum: 20,
	},
	{
		name:         "noop_last",
		pending:      nil,
		clientSeqNum: 42,
		serverSeqNum: 42,
	},
	{
		name:         "noop_client_oob",
		pending:      nil,
		clientSeqNum: 43,
		serverSeqNum: 0,
	},
	{
		name:         "noop_server_oob",
		pending:      nil,
		clientSeqNum: 0,
		serverSeqNum: 43,
	},
	{
		name:         "expunge_eq",
		pending:      []trackerUpdate{{expunge: 20}},
		clientSeqNum: 20,
		serverSeqNum: 0,
	},
	{
		name:         "expunge_lt",
		pending:      []trackerUpdate{{expunge: 20}},
		clientSeqNum: 10,
		serverSeqNum: 10,
	},
	{
		name:         "expunge_gt",
		pending:      []trackerUpdate{{expunge: 10}},
		clientSeqNum: 20,
		serverSeqNum: 19,
	},
	{
		name:         "append_eq",
		pending:      []trackerUpdate{{numMessages: 43}},
		clientSeqNum: 0,
		serverSeqNum: 43,
	},
	{
		name:         "append_lt",
		pending:      []trackerUpdate{{numMessages: 43}},
		clientSeqNum: 42,
		serverSeqNum: 42,
	},
	{
		name: "expunge_append",
		pending: []trackerUpdate{
			{expunge: 42},
			{numMessages: 42},
		},
		clientSeqNum: 42,
		serverSeqNum: 0,
	},
	{
		name: "expunge_append",
		pending: []trackerUpdate{
			{expunge: 42},
			{numMessages: 42},
		},
		clientSeqNum: 0,
		serverSeqNum: 42,
	},
	{
		name: "append_expunge",
		pending: []trackerUpdate{
			{numMessages: 43},
			{expunge: 42},
		},
		clientSeqNum: 42,
		serverSeqNum: 0,
	},
	{
		name: "append_expunge",
		pending: []trackerUpdate{
			{numMessages: 43},
			{expunge: 42},
		},
		clientSeqNum: 0,
		serverSeqNum: 42,
	},
	{
		name: "multi_expunge_middle",
		pending: []trackerUpdate{
			{expunge: 3},
			{expunge: 1},
		},
		clientSeqNum: 2,
		serverSeqNum: 1,
	},
	{
		name: "multi_expunge_after",
		pending: []trackerUpdate{
			{expunge: 3},
			{expunge: 1},
		},
		clientSeqNum: 4,
		serverSeqNum: 2,
	},
}

func TestSessionTracker(t *testing.T) {
	for _, tc := range sessionTrackerSeqNumTests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			mboxTracker := imapserver.NewMailboxTracker(42)
			sessTracker := mboxTracker.NewSession()
			for _, update := range tc.pending {
				switch {
				case update.expunge != 0:
					mboxTracker.QueueExpunge(update.expunge, 0)
				case update.numMessages != 0:
					mboxTracker.QueueNumMessages(update.numMessages)
				}
			}

			serverSeqNum := sessTracker.DecodeSeqNum(tc.clientSeqNum)
			if tc.clientSeqNum != 0 && serverSeqNum != tc.serverSeqNum {
				t.Errorf("DecodeSeqNum(%v): got %v, want %v", tc.clientSeqNum, serverSeqNum, tc.serverSeqNum)
			}

			clientSeqNum := sessTracker.EncodeSeqNum(tc.serverSeqNum)
			if tc.serverSeqNum != 0 && clientSeqNum != tc.clientSeqNum {
				t.Errorf("EncodeSeqNum(%v): got %v, want %v", tc.serverSeqNum, clientSeqNum, tc.clientSeqNum)
			}
		})
	}
}

// TestSessionTrackerEncodeNumMessages covers the value "*" resolves to in a
// sequence set. The mailbox starts with 42 messages, all of them already known
// to the client, and each case then queues updates the client has not seen.
func TestSessionTrackerEncodeNumMessages(t *testing.T) {
	tests := []struct {
		name    string
		pending []trackerUpdate
		want    uint32
	}{
		{
			name: "no pending updates",
			want: 42,
		},
		{
			// The client cannot name a message it has not been told about.
			name:    "one pending append",
			pending: []trackerUpdate{{numMessages: 43}},
			want:    42,
		},
		{
			// A single update announcing three messages at once must be undone
			// as three, not as one. Deriving the count by decrementing per
			// update gets this wrong.
			name:    "one pending append of three messages",
			pending: []trackerUpdate{{numMessages: 45}},
			want:    42,
		},
		{
			name:    "several pending appends",
			pending: []trackerUpdate{{numMessages: 43}, {numMessages: 46}},
			want:    42,
		},
		{
			// The expunge has not been dispatched, so from the client's point
			// of view the message is still there and still addressable.
			name:    "one pending expunge",
			pending: []trackerUpdate{{expunge: 1}},
			want:    42,
		},
		{
			name:    "two pending expunges",
			pending: []trackerUpdate{{expunge: 1}, {expunge: 1}},
			want:    42,
		},
		{
			name:    "append then expunge",
			pending: []trackerUpdate{{numMessages: 43}, {expunge: 1}},
			want:    42,
		},
		{
			name:    "expunge then append",
			pending: []trackerUpdate{{expunge: 1}, {numMessages: 42}},
			want:    42,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mboxTracker := imapserver.NewMailboxTracker(42)
			sessTracker := mboxTracker.NewSession()
			for _, update := range tc.pending {
				switch {
				case update.expunge != 0:
					if err := mboxTracker.QueueExpunge(update.expunge, 0); err != nil {
						t.Fatalf("QueueExpunge(%v): %v", update.expunge, err)
					}
				case update.numMessages != 0:
					if err := mboxTracker.QueueNumMessages(update.numMessages); err != nil {
						t.Fatalf("QueueNumMessages(%v): %v", update.numMessages, err)
					}
				}
			}

			if got := sessTracker.EncodeNumMessages(); got != tc.want {
				t.Errorf("EncodeNumMessages() = %v, want %v", got, tc.want)
			}
		})
	}
}
