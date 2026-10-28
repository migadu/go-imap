package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RFC 4731 §3.1 (RFC 9051 §7.3.4): esearch-response = "ESEARCH"
// [search-correlator] [SP "UID"] *(SP search-return-data). The correlator is
// optional, and so is everything after it.
func TestESearchWithoutCorrelator(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uid       bool
		response  string
		wantAll   string
		wantCount uint32
	}{
		{name: "ALL", response: "* ESEARCH ALL 1:3", wantAll: "1:3"},
		{name: "UID ALL", uid: true, response: "* ESEARCH UID ALL 4,7", wantAll: "4,7"},
		{name: "COUNT", response: "* ESEARCH COUNT 3", wantCount: 3},
		{name: "bare", response: "* ESEARCH"},
		{name: "bare UID", uid: true, response: "* ESEARCH UID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newESearchServer(t, tc.response)

			criteria := &imap.SearchCriteria{}
			options := &imap.SearchOptions{ReturnAll: true, ReturnCount: true}
			var cmd *imapclient.SearchCommand
			if tc.uid {
				cmd = client.UIDSearch(criteria, options)
			} else {
				cmd = client.Search(criteria, options)
			}
			data, err := cmd.Wait()
			if err != nil {
				t.Fatalf("Search() = %v", err)
			}
			var gotAll string
			if data.All != nil {
				gotAll = data.All.String()
			}
			if gotAll != tc.wantAll || data.Count != tc.wantCount {
				t.Errorf("ALL %q COUNT %v, want ALL %q COUNT %v", gotAll, data.Count, tc.wantAll, tc.wantCount)
			}
		})
	}

	t.Run("SORT", func(t *testing.T) {
		client := newESearchServer(t, "* ESEARCH ALL 3,1,2")
		data, err := client.Sort(&imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
			SearchCriteria: &imap.SearchCriteria{},
			Return:         imap.SortOptions{ReturnAll: true},
		}).Wait()
		if err != nil {
			t.Fatalf("Sort() = %v", err)
		}
		if want := []uint32{3, 1, 2}; !reflect.DeepEqual(data.SeqNums, want) {
			t.Errorf("SortData.SeqNums = %v, want %v", data.SeqNums, want)
		}
	})
}

// newESearchServer answers one command with response and a tagged OK.
func newESearchServer(t *testing.T, response string) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { serverConn.Close() })
	go func() {
		fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1 ESEARCH SORT ESORT] ready\r\n")
		line, err := bufio.NewReader(serverConn).ReadString('\n')
		if err != nil {
			return
		}
		tag, _, _ := strings.Cut(line, " ")
		fmt.Fprint(serverConn, response+"\r\n"+tag+" OK done\r\n")
	}()
	client := imapclient.New(clientConn, nil)
	t.Cleanup(func() { client.Close() })
	return client
}
