package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestSort(t *testing.T) {
	// Messages with varying attributes
	// msg1: smallest, oldest arrival, subject "C", From: B, To: Y
	// msg2: medium, middle arrival, subject "B", From: C, To: X
	// msg3: largest, newest arrival, subject "A", From: A, To: Z
	msg1 := []byte("From: B <b@example.org>\r\nTo: Y <y@example.org>\r\nDate: Mon, 2 Jan 2006 15:04:05 -0700\r\nSubject: C\r\n\r\nSmall")
	msg2 := []byte("From: C <c@example.org>\r\nTo: X <x@example.org>\r\nDate: Tue, 3 Jan 2006 15:04:05 -0700\r\nSubject: B\r\n\r\nMedium body")
	msg3 := []byte("From: A <a@example.org>\r\nTo: Z <z@example.org>\r\nDate: Wed, 4 Jan 2006 15:04:05 -0700\r\nSubject: A\r\n\r\nMuch larger body content here")

	client, server := newClientServerPair(t, imap.ConnStateSelected)

	defer client.Close()
	defer server.Close()

	const mboxName = "SORT-TEST"
	if err := client.Create(mboxName, nil).Wait(); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	if _, err := client.Select(mboxName, nil).Wait(); err != nil {
		t.Fatalf("Select() failed: %v", err)
	}

	appendMsg := func(msgData []byte) {
		cmd := client.Append(mboxName, int64(len(msgData)), nil)
		if _, err := cmd.Write(msgData); err != nil {
			t.Fatalf("Append.Write() failed: %v", err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("Append.Close() failed: %v", err)
		}
		if _, err := cmd.Wait(); err != nil {
			t.Fatalf("Append.Wait() failed: %v", err)
		}
	}

	// Append messages, ensuring different arrival times
	appendMsg(msg1)
	time.Sleep(10 * time.Millisecond)
	appendMsg(msg2)
	time.Sleep(10 * time.Millisecond)
	appendMsg(msg3)

	allMsgs := &imap.SearchCriteria{}

	t.Run("SORT", func(t *testing.T) {
		tests := []struct {
			name    string
			options *imapclient.SortOptions
			want    []uint32
		}{
			{
				name: "ARRIVAL",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{1, 2, 3},
			},
			{
				name: "REVERSE ARRIVAL",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: true}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{3, 2, 1},
			},
			{
				name: "SIZE",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeySize}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{1, 2, 3},
			},
			{
				name: "SUBJECT",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeySubject}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{3, 2, 1}, // A, B, C
			},
			{
				name: "DISPLAYFROM",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDisplayFrom}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{3, 1, 2}, // A, B, C
			},
			{
				name: "DISPLAYTO",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDisplayTo}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{2, 1, 3}, // X, Y, Z
			},
			{
				name: "FROM",
				options: &imapclient.SortOptions{
					SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyFrom}},
					SearchCriteria: allMsgs,
				},
				want: []uint32{3, 1, 2}, // a@, b@, c@
			},
			{
				name: "DATE then SUBJECT",
				options: &imapclient.SortOptions{
					SortCriteria: []imap.SortCriterion{
						{Key: imap.SortKeyDate},
						{Key: imap.SortKeySubject}, // tie-breaker, but not needed here
					},
					SearchCriteria: allMsgs,
				},
				want: []uint32{1, 2, 3},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				data, err := client.Sort(tt.options).Wait()
				if err != nil {
					t.Fatalf("Sort() failed: %v", err)
				}
				if !reflect.DeepEqual(data.SeqNums, tt.want) {
					t.Errorf("Sort() SeqNums = %v, want %v", data.SeqNums, tt.want)
				}
				if len(data.UIDs) > 0 {
					t.Errorf("Sort() returned UIDs unexpectedly: %v", data.UIDs)
				}
			})
		}
	})

	t.Run("UID SORT", func(t *testing.T) {
		options := &imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: true}},
			SearchCriteria: allMsgs,
		}
		data, err := client.UIDSort(options).Wait()
		if err != nil {
			t.Fatalf("UIDSort() failed: %v", err)
		}

		wantUIDs := []imap.UID{3, 2, 1}
		if !reflect.DeepEqual(data.UIDs, wantUIDs) {
			t.Errorf("UIDSort() UIDs = %v, want %v", data.UIDs, wantUIDs)
		}
		if len(data.SeqNums) > 0 {
			t.Errorf("UIDSort() returned SeqNums unexpectedly: %v", data.SeqNums)
		}
	})

	t.Run("ESORT", func(t *testing.T) {
		options := &imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
			SearchCriteria: allMsgs,
			Return: imap.SortOptions{
				ReturnCount: true,
				ReturnMin:   true,
				ReturnMax:   true,
				ReturnAll:   true,
			},
		}
		data, err := client.Sort(options).Wait()
		if err != nil {
			t.Fatalf("ESort() failed: %v", err)
		}

		if data.Count != 3 {
			t.Errorf("ESort() Count = %v, want 3", data.Count)
		}
		if data.Min != 1 {
			t.Errorf("ESort() Min = %v, want 1", data.Min)
		}
		if data.Max != 3 {
			t.Errorf("ESort() Max = %v, want 3", data.Max)
		}
		wantSeqNums := []uint32{1, 2, 3}
		if !reflect.DeepEqual(data.SeqNums, wantSeqNums) {
			t.Errorf("ESort() SeqNums = %v, want %v", data.SeqNums, wantSeqNums)
		}

		// Reverse sort order must be preserved in ALL (3, 2, 1), not sorted back
		// into ascending numerical order.
		revOptions := &imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: true}},
			SearchCriteria: allMsgs,
			Return: imap.SortOptions{
				ReturnCount: true,
				ReturnMin:   true,
				ReturnMax:   true,
				ReturnAll:   true,
			},
		}
		revData, err := client.Sort(revOptions).Wait()
		if err != nil {
			t.Fatalf("ESort(REVERSE) failed: %v", err)
		}
		wantRevSeq := []uint32{3, 2, 1}
		if !reflect.DeepEqual(revData.SeqNums, wantRevSeq) {
			t.Errorf("ESort(REVERSE) SeqNums = %v, want %v", revData.SeqNums, wantRevSeq)
		}

		revUIDData, err := client.UIDSort(revOptions).Wait()
		if err != nil {
			t.Fatalf("UIDSort(REVERSE RETURN) failed: %v", err)
		}
		wantRevUIDs := []imap.UID{3, 2, 1}
		if !reflect.DeepEqual(revUIDData.UIDs, wantRevUIDs) {
			t.Errorf("UIDSort(REVERSE RETURN) UIDs = %v, want %v", revUIDData.UIDs, wantRevUIDs)
		}
	})

	t.Run("ESORT REVERSE", func(t *testing.T) {
		options := &imapclient.SortOptions{
			SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival, Reverse: true}},
			SearchCriteria: allMsgs,
			Return:         imap.SortOptions{ReturnAll: true},
		}
		data, err := client.Sort(options).Wait()
		if err != nil {
			t.Fatalf("Sort() failed: %v", err)
		}
		// RFC 5267 §3.1: ALL lists the messages in sort order.
		wantSeqNums := []uint32{3, 2, 1}
		if !reflect.DeepEqual(data.SeqNums, wantSeqNums) {
			t.Errorf("Sort() SeqNums = %v, want %v", data.SeqNums, wantSeqNums)
		}
	})

	t.Run("Empty result", func(t *testing.T) {
		// Search for a non-existent message
		options := &imapclient.SortOptions{
			SortCriteria: []imap.SortCriterion{{Key: imap.SortKeyArrival}},
			SearchCriteria: &imap.SearchCriteria{
				Header: []imap.SearchCriteriaHeaderField{{Key: "Subject", Value: "non-existent"}},
			},
			Return: imap.SortOptions{
				ReturnCount: true,
				ReturnAll:   true,
			},
		}

		data, err := client.Sort(options).Wait()
		if err != nil {
			t.Fatalf("Sort() with empty result failed: %v", err)
		}

		if data.Count != 0 {
			t.Errorf("Count = %v, want 0", data.Count)
		}
		if len(data.SeqNums) != 0 {
			t.Errorf("SeqNums = %v, want empty", data.SeqNums)
		}
	})

	// RFC 5267 compliance tests:
	// - Empty RETURN list defaults to ALL
	// - Unknown RETURN options are silently ignored
	// These are tested at the server/protocol level in imapserver package
}

// TestESortWire verifies RFC 5267 §3.2 ordered ALL range expansion (12:10 is
// equivalent to 10:12, while comma-separated tokens preserve wire order), and
// that an uncorrelated ESEARCH, which no command caused (RFC 4466 §2.6.2), is
// read without desynchronising the stream and without landing in a SORT's
// result.
func TestESortWire(t *testing.T) {
	tests := []struct {
		name     string
		uid      bool
		wireLine func(tag string) string
		wantUIDs []imap.UID
		wantSeqs []uint32
		wantMin  uint32
		wantMax  uint32
		wantCnt  uint32
	}{
		{
			name: "correlated UID with descending range token",
			uid:  true,
			wireLine: func(tag string) string {
				return fmt.Sprintf("* ESEARCH (TAG %q) UID ALL 30,12:10,5\r\n", tag)
			},
			wantUIDs: []imap.UID{30, 10, 11, 12, 5},
		},
		{
			name: "correlated seqnum with descending range token",
			uid:  false,
			wireLine: func(tag string) string {
				return fmt.Sprintf("* ESEARCH (TAG %q) ALL 12:10\r\n", tag)
			},
			wantSeqs: []uint32{10, 11, 12},
		},
		{
			name: "uncorrelated UID with MIN MAX COUNT ALL",
			uid:  true,
			wireLine: func(tag string) string {
				return "* ESEARCH UID MIN 30 MAX 5 COUNT 5 ALL 30,12:10,5\r\n"
			},
		},
		{
			name: "uncorrelated seqnum with descending range token",
			uid:  false,
			wireLine: func(tag string) string {
				return "* ESEARCH ALL 12:10\r\n"
			},
		},
		{
			name: "uncorrelated UID empty",
			uid:  true,
			wireLine: func(tag string) string {
				return "* ESEARCH UID\r\n"
			},
		},
		{
			name: "uncorrelated seqnum empty",
			uid:  false,
			wireLine: func(tag string) string {
				return "* ESEARCH\r\n"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			t.Cleanup(func() { clientConn.Close(); serverConn.Close() })

			go func() {
				defer serverConn.Close()
				io.WriteString(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 SORT ESORT] ready\r\n")
				reader := bufio.NewReader(serverConn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					tag := fields[0]
					if strings.Contains(line, " SORT ") {
						io.WriteString(serverConn, tc.wireLine(tag))
					}
					fmt.Fprintf(serverConn, "%s OK completed\r\n", tag)
				}
			}()

			client := imapclient.New(clientConn, nil)
			t.Cleanup(func() { client.Close() })
			if err := client.WaitGreeting(); err != nil {
				t.Fatalf("WaitGreeting() = %v", err)
			}

			opts := &imapclient.SortOptions{
				SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyArrival}},
				SearchCriteria: &imap.SearchCriteria{},
				Return:         imap.SortOptions{ReturnAll: true},
			}
			var (
				data *imapclient.SortData
				err  error
			)
			if tc.uid {
				data, err = client.UIDSort(opts).Wait()
			} else {
				data, err = client.Sort(opts).Wait()
			}
			if err != nil {
				t.Fatalf("Sort().Wait() = %v", err)
			}

			if data.Min != tc.wantMin {
				t.Errorf("Min = %d, want %d", data.Min, tc.wantMin)
			}
			if data.Max != tc.wantMax {
				t.Errorf("Max = %d, want %d", data.Max, tc.wantMax)
			}
			if data.Count != tc.wantCnt {
				t.Errorf("Count = %d, want %d", data.Count, tc.wantCnt)
			}
			if !reflect.DeepEqual(data.UIDs, tc.wantUIDs) {
				t.Errorf("UIDs = %v, want %v", data.UIDs, tc.wantUIDs)
			}
			if !reflect.DeepEqual(data.SeqNums, tc.wantSeqs) {
				t.Errorf("SeqNums = %v, want %v", data.SeqNums, tc.wantSeqs)
			}

			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop().Wait() after ESORT failed (stream desync): %v", err)
			}
		})
	}
}
