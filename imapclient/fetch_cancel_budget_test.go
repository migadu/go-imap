package imapclient

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func TestCancelledUndeliveredFetchReleasesBudget(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	closeCh := make(chan struct{})
	close(closeCh)

	// The body precedes UID, so it must be buffered before routing this
	// response. No receiver can accept the message once its UID is known.
	cmd := &FetchCommand{
		numSet: imap.UIDSetNum(42),
		msgs:   make(chan *FetchMessageData),
	}
	c := &Client{
		conn:        clientConn,
		closeCh:     closeCh,
		state:       imap.ConnStateSelected,
		pendingCmds: []command{cmd},
		options:     Options{MaxFetchBufferSize: 4},
	}
	c.dec = imapwire.NewDecoder(bufio.NewReader(strings.NewReader("(BODY[] {4}\r\ndata UID 42)")), imapwire.ConnSideClient)

	if err := c.handleFetch(1); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("handleFetch() = %v, want cancellation", err)
	}
	if c.fetchBufInUse != 0 {
		t.Fatalf("an undelivered FETCH message retains %d buffered bytes", c.fetchBufInUse)
	}
}
