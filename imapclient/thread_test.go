package imapclient_test

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestThread(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.Cap("THREAD=REFERENCES")) {
		t.Skip("server doesn't support THREAD=REFERENCES")
	}

	options := &imapclient.ThreadOptions{
		Algorithm: imap.ThreadReferences,
		SearchCriteria: &imap.SearchCriteria{
			SeqNum: []imap.SeqSet{
				imap.SeqSetNum(1), // Match message 1
			},
		},
	}

	cmd := client.Thread(options)
	data, err := cmd.Wait()
	if err != nil {
		t.Fatalf("Thread().Wait() = %v", err)
	}

	// We'd expect some thread data back, e.g. [{Chain:[1], SubThreads:[]}]
	want := []imap.ThreadData{
		{Chain: []uint32{1}},
	}

	if !reflect.DeepEqual(data, want) {
		t.Errorf("ThreadData = %v, want %v", data, want)
	}
}
