package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// A QRESYNC SELECT can be answered with more than one VANISHED (EARLIER)
// response: RFC 7162 §3.2.10 has a server report the earlier and the live form
// in separate responses, and nothing caps the expunged set at a single one.
// Every UID reported has to reach SelectData, or the client leaves messages
// behind that the server has expunged.
func TestSelect_QResync_MultipleVanishedEarlier(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() {
		client.Close()
		serverConn.Close()
	})

	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2 QRESYNC] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		io.WriteString(serverConn, "* VANISHED (EARLIER) 1:3\r\n")
		io.WriteString(serverConn, "* VANISHED (EARLIER) 7,9:11\r\n")
		io.WriteString(serverConn, "* 0 EXISTS\r\n")
		io.WriteString(serverConn, tag+" OK [READ-WRITE] SELECT completed\r\n")
	}()

	data, err := client.Select("INBOX", &imap.SelectOptions{
		QResync: &imap.QResyncData{UIDValidity: 1, ModSeq: 1},
	}).Wait()
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}

	for _, uid := range []imap.UID{1, 2, 3, 7, 9, 10, 11} {
		if !data.Vanished.Contains(uid) {
			t.Errorf("SelectData.Vanished = %v, missing UID %v", data.Vanished, uid)
		}
	}
	for _, uid := range []imap.UID{4, 8, 12} {
		if data.Vanished.Contains(uid) {
			t.Errorf("SelectData.Vanished = %v, should not contain UID %v", data.Vanished, uid)
		}
	}
}

// RFC 7162 §3.2.10 forbids a server from combining the two forms of VANISHED,
// because a client treats them differently. A live VANISHED arriving while a
// QRESYNC SELECT is in flight reports an expunge happening now, not part of the
// resynchronization, so it belongs to the unilateral handler rather than to
// SelectData.
func TestSelect_QResync_LiveVanishedIsNotResync(t *testing.T) {
	clientConn, serverConn := net.Pipe()

	var (
		mu   sync.Mutex
		live imap.UIDSet
	)
	client := imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Vanished: func(data *imap.VanishedData) {
				mu.Lock()
				if !data.Earlier {
					live = append(live, data.UIDs...)
				}
				mu.Unlock()
			},
		},
	})
	t.Cleanup(func() {
		client.Close()
		serverConn.Close()
	})

	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev2 QRESYNC] ready\r\n")
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		io.WriteString(serverConn, "* VANISHED (EARLIER) 1:3\r\n")
		io.WriteString(serverConn, "* VANISHED 7\r\n")
		io.WriteString(serverConn, tag+" OK [READ-WRITE] SELECT completed\r\n")
	}()

	data, err := client.Select("INBOX", &imap.SelectOptions{
		QResync: &imap.QResyncData{UIDValidity: 1, ModSeq: 1},
	}).Wait()
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}

	if !data.Vanished.Contains(1) || !data.Vanished.Contains(3) {
		t.Errorf("SelectData.Vanished = %v, want the earlier set 1:3", data.Vanished)
	}
	if data.Vanished.Contains(7) {
		t.Errorf("SelectData.Vanished = %v, want the live expunge of UID 7 kept out", data.Vanished)
	}

	mu.Lock()
	defer mu.Unlock()
	if !live.Contains(7) {
		t.Errorf("unilateral Vanished handler got %v, want the live expunge of UID 7", live)
	}
}
