package imapclient_test

import (
	"reflect"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
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

// TestThreadResponseParsing checks that the client reads THREAD responses in
// the RFC 5256 section 5 form, where thread-lists follow one another with
// nothing between them, and in the SP-separated form go-imap servers sent
// before. Both parse because Decoder.SP accepts a missing SP before '('; that
// leniency is also why a client test against the server never caught the
// server sending the SP-separated form.
func TestThreadResponseParsing(t *testing.T) {
	rfcExample := []imap.ThreadData{
		{Chain: []uint32{2}},
		{Chain: []uint32{3, 6}, SubThreads: []imap.ThreadData{
			{Chain: []uint32{4, 23}},
			{Chain: []uint32{44, 7, 96}},
		}},
		{SubThreads: []imap.ThreadData{
			{Chain: []uint32{11}},
			{Chain: []uint32{12}},
		}},
	}

	tests := []struct {
		name string
		resp string
		want []imap.ThreadData
	}{
		{"empty", "* THREAD\r\n", nil},
		{"RFC 5256 form", "* THREAD (2)(3 6 (4 23)(44 7 96))((11)(12))\r\n", rfcExample},
		{"SP-separated form", "* THREAD (2) (3 6 (4 23) (44 7 96)) ((11) (12))\r\n", rfcExample},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newCodeServer(t, tc.resp)
			data, err := client.UIDThread(&imapclient.ThreadOptions{
				Algorithm:      imap.ThreadReferences,
				SearchCriteria: &imap.SearchCriteria{},
			}).Wait()
			if err != nil {
				t.Fatalf("UIDThread().Wait() = %v", err)
			}
			if !reflect.DeepEqual(data, tc.want) {
				t.Errorf("ThreadData = %+v, want %+v", data, tc.want)
			}
		})
	}
}
