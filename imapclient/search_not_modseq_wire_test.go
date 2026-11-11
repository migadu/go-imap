package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// The commands TestSort_ModSeq's "MODSEQ under NOT" cases send, as they go on
// the wire. Dovecot 2.4.5 answers them wrongly (#157), which leaves only this
// to check their encoding when the tests run against it.
func TestNotModSeqWire(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

	lines := make(chan string, 4)
	go func() {
		defer serverConn.Close()
		io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 SORT CONDSTORE] ready\r\n")
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			tag, cmd, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
			lines <- cmd
			fmt.Fprintf(serverConn, "%s OK done\r\n", tag)
		}
	}()

	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	if err := client.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}

	criteria := &imap.SearchCriteria{Not: []imap.SearchCriteria{{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 4}}}}
	if _, err := client.Search(criteria, nil).Wait(); err != nil {
		t.Fatalf("Search() = %v", err)
	}
	if got, want := <-lines, "SEARCH NOT (MODSEQ 4)"; got != want {
		t.Errorf("Search() sent %q, want %q", got, want)
	}
	if _, err := client.Sort(&imapclient.SortOptions{
		SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
		SearchCriteria: criteria,
	}).Wait(); err != nil {
		t.Fatalf("Sort() = %v", err)
	}
	if got, want := <-lines, "SORT (ARRIVAL) UTF-8 NOT (MODSEQ 4)"; got != want {
		t.Errorf("Sort() sent %q, want %q", got, want)
	}
}
