package imapserver

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// unselectRecordingSession implements the base Session interface via a nil
// embedded Session (never called) and overrides only the methods exercised by
// the SELECT/EXAMINE/CLOSE/UNSELECT paths.
type unselectRecordingSession struct {
	Session
	selectData     *imap.SelectData
	expungeCalled  bool
	expungeErr     error
	unselectCalled bool
}

func (s *unselectRecordingSession) Select(ctx context.Context, mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	return s.selectData, nil
}

func (s *unselectRecordingSession) Unselect(ctx context.Context) error {
	s.unselectCalled = true
	return nil
}

func (s *unselectRecordingSession) Expunge(ctx context.Context, w *ExpungeWriter, uids *imap.UIDSet) error {
	s.expungeCalled = true
	return s.expungeErr
}

func newUnselectTestConn(t *testing.T, session Session) *Conn {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	return &Conn{
		conn:    c1,
		session: session,
		state:   imap.ConnStateSelected,
		server:  New(&Options{}),
		bw:      bufio.NewWriter(io.Discard),
	}
}

func crlfDecoder() *imapwire.Decoder {
	return imapwire.NewDecoder(bufio.NewReader(strings.NewReader("\r\n")), imapwire.ConnSideServer)
}

// TestCloseSkipsExpungeOnReadOnlyMailbox is a regression test for RFC 3501
// §6.4.2: "No messages are removed, and no error is given, if the mailbox is
// selected by an EXAMINE command or is otherwise selected read-only."
// Previously CLOSE always invoked Session.Expunge; a backend correctly
// rejecting EXPUNGE on a read-only mailbox turned CLOSE into a tagged NO and
// left the connection stuck in the selected state.
func TestCloseSkipsExpungeOnReadOnlyMailbox(t *testing.T) {
	tests := []struct {
		name              string
		selectedReadOnly  bool
		expunge           bool // true = CLOSE, false = UNSELECT
		wantExpungeCalled bool
	}{
		{
			name:              "CLOSE on read-only mailbox skips expunge",
			selectedReadOnly:  true,
			expunge:           true,
			wantExpungeCalled: false,
		},
		{
			name:              "CLOSE on read-write mailbox expunges",
			selectedReadOnly:  false,
			expunge:           true,
			wantExpungeCalled: true,
		},
		{
			name:              "UNSELECT never expunges",
			selectedReadOnly:  false,
			expunge:           false,
			wantExpungeCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &unselectRecordingSession{}
			conn := newUnselectTestConn(t, session)
			conn.selectedReadOnly = tt.selectedReadOnly

			if err := conn.handleUnselect(crlfDecoder(), tt.expunge); err != nil {
				t.Fatalf("handleUnselect: unexpected error: %v", err)
			}

			if session.expungeCalled != tt.wantExpungeCalled {
				t.Errorf("Expunge called = %v, want %v", session.expungeCalled, tt.wantExpungeCalled)
			}
			if !session.unselectCalled {
				t.Error("Unselect was not called")
			}
			if conn.state != imap.ConnStateAuthenticated {
				t.Errorf("state = %v, want %v", conn.state, imap.ConnStateAuthenticated)
			}
			if conn.selectedReadOnly {
				t.Error("selectedReadOnly not cleared after unselect")
			}
		})
	}
}

// TestCloseExpungeErrorHandling verifies the RFC 4314 §4 carve-out is
// preserved: CLOSE ignores a NOPERM expunge failure (missing "e" right) but
// still fails on other expunge errors.
func TestCloseExpungeErrorHandling(t *testing.T) {
	tests := []struct {
		name       string
		expungeErr error
		wantErr    bool
	}{
		{
			name: "NOPERM expunge failure is ignored",
			expungeErr: &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeNoPerm,
				Text: "missing e right",
			},
			wantErr: false,
		},
		{
			name: "other expunge failures propagate",
			expungeErr: &imap.Error{
				Type: imap.StatusResponseTypeNo,
				Code: imap.ResponseCodeServerBug,
				Text: "boom",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &unselectRecordingSession{expungeErr: tt.expungeErr}
			conn := newUnselectTestConn(t, session)

			err := conn.handleUnselect(crlfDecoder(), true)
			if (err != nil) != tt.wantErr {
				t.Fatalf("handleUnselect error = %v, wantErr %v", err, tt.wantErr)
			}
			if !session.expungeCalled {
				t.Fatal("Expunge was not called")
			}
			if session.unselectCalled != !tt.wantErr {
				t.Errorf("Unselect called = %v, want %v", session.unselectCalled, !tt.wantErr)
			}
		})
	}
}

// TestSelectSetsSelectedReadOnly verifies the connection records read-only
// status on SELECT/EXAMINE: EXAMINE is always read-only, and SELECT is
// read-only when the session reports data.ReadOnly (RFC 4314 §5.2).
func TestSelectSetsSelectedReadOnly(t *testing.T) {
	tests := []struct {
		name         string
		examine      bool
		dataReadOnly bool
		want         bool
	}{
		{name: "EXAMINE", examine: true, dataReadOnly: false, want: true},
		{name: "SELECT with read-only ACL", examine: false, dataReadOnly: true, want: true},
		{name: "SELECT read-write", examine: false, dataReadOnly: false, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &unselectRecordingSession{
				selectData: &imap.SelectData{ReadOnly: tt.dataReadOnly},
			}
			conn := newUnselectTestConn(t, session)
			// Start selected read-only to prove re-selecting recomputes the
			// flag rather than leaving the previous mailbox's value behind.
			conn.selectedReadOnly = true

			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(" INBOX\r\n")), imapwire.ConnSideServer)
			if err := conn.handleSelect("T1", dec, tt.examine); err != nil {
				t.Fatalf("handleSelect: unexpected error: %v", err)
			}

			if conn.state != imap.ConnStateSelected {
				t.Fatalf("state = %v, want %v", conn.state, imap.ConnStateSelected)
			}
			if conn.selectedReadOnly != tt.want {
				t.Errorf("selectedReadOnly = %v, want %v", conn.selectedReadOnly, tt.want)
			}
		})
	}
}
