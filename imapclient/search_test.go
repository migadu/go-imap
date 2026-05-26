package imapclient_test

import (
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestSearch(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	criteria := imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   "Message-Id",
			Value: "<191101702316132@example.com>",
		}},
	}
	data, err := client.Search(&criteria, nil).Wait()
	if err != nil {
		t.Fatalf("Search().Wait() = %v", err)
	}
	seqSet, ok := data.All.(imap.SeqSet)
	if !ok {
		t.Fatalf("SearchData.All = %T, want SeqSet", data.All)
	}
	nums, _ := seqSet.Nums()
	want := []uint32{1}
	if !reflect.DeepEqual(nums, want) {
		t.Errorf("SearchData.All.Nums() = %v, want %v", nums, want)
	}
}

func TestESearch(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.CapESearch) {
		t.Skip("server doesn't support ESEARCH")
	}

	criteria := imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   "Message-Id",
			Value: "<191101702316132@example.com>",
		}},
	}
	options := imap.SearchOptions{
		ReturnCount: true,
	}
	data, err := client.Search(&criteria, &options).Wait()
	if err != nil {
		t.Fatalf("Search().Wait() = %v", err)
	}
	if want := uint32(1); data.Count != want {
		t.Errorf("Count = %v, want %v", data.Count, want)
	}
}

func TestMultiSearch(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateSelected)
	defer client.Close()
	defer server.Close()

	if !client.Caps().Has(imap.Cap("MULTISEARCH")) {
		t.Skip("server doesn't support MULTISEARCH")
	}

	// Create archive mailbox and add the same message
	if err := client.Create("archive", nil).Wait(); err != nil {
		t.Fatalf("Create().Wait() = %v", err)
	}
	appendCmd := client.Append("archive", int64(len(simpleRawMessage)), nil)
	appendCmd.Write([]byte(simpleRawMessage))
	appendCmd.Close()
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("Append().Wait() = %v", err)
	}

	criteria := imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{
			Key:   "Message-Id",
			Value: "<191101702316132@example.com>",
		}},
	}
	options := imap.SearchOptions{
		ReturnCount: true,
	}
	dataList, err := client.MultiSearch([]string{"INBOX", "archive"}, &criteria, &options).Wait()
	if err != nil {
		t.Fatalf("MultiSearch().Wait() = %v", err)
	}
	if len(dataList) != 2 {
		t.Fatalf("len(dataList) = %v, want 2", len(dataList))
	}
	if want := uint32(1); dataList[0].Count != want {
		t.Errorf("dataList[0].Count = %v, want %v", dataList[0].Count, want)
	}
	if want := "INBOX"; dataList[0].Mailbox != want {
		t.Errorf("dataList[0].Mailbox = %v, want %v", dataList[0].Mailbox, want)
	}
	if want := uint32(1); dataList[1].Count != want {
		t.Errorf("dataList[1].Count = %v, want %v", dataList[1].Count, want)
	}
	if want := "archive"; dataList[1].Mailbox != want {
		t.Errorf("dataList[1].Mailbox = %v, want %v", dataList[1].Mailbox, want)
	}
}
