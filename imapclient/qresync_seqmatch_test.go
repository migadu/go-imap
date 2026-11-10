package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// seq-match-data is "known-sequence-set SP known-uid-set" (RFC 7162 §3.2.5):
// the first half carries sequence numbers, the second the UIDs they map to.
func TestSelect_QResync_SeqMatchData(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() {
		client.Close()
		serverConn.Close()
	})

	gotCmd := make(chan string, 1)
	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2 QRESYNC] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		gotCmd <- line
		tag, _, _ := strings.Cut(line, " ")
		io.WriteString(serverConn, tag+" OK [READ-WRITE] SELECT completed\r\n")
	}()

	_, err := client.Select("INBOX", &imap.SelectOptions{
		QResync: &imap.QResyncData{
			UIDValidity: 67890,
			ModSeq:      12345,
			KnownUIDs:   imap.UIDSetNum(1, 2, 3),
			SeqMatch: &imap.QResyncSeqMatch{
				SeqNums: imap.SeqSetNum(1, 2, 3, 4, 5),
				UIDs:    imap.UIDSetNum(10, 11, 12),
			},
		},
	}).Wait()
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}

	const want = "(QRESYNC (67890 12345 1:3 (1:5 10:12)))"
	if got := <-gotCmd; !strings.Contains(got, want) {
		t.Errorf("SELECT command = %q, want it to contain %q", strings.TrimSpace(got), want)
	}
}
