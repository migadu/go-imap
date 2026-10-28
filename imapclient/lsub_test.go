package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func mailboxNames(mailboxes []*imap.ListData) []string {
	var names []string
	for _, mbox := range mailboxes {
		names = append(names, mbox.Mailbox)
	}
	sort.Strings(names)
	return names
}

// LSUB (RFC 3501 §6.3.9) lists the subscribed mailboxes. IMAP4rev2 replaces it
// with LIST (SUBSCRIBED), but that is LIST-EXTENDED syntax, so LSUB is the
// only way to learn the subscriptions of an IMAP4rev1 server without
// LIST-EXTENDED.
func TestLsub(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	for _, name := range []string{"Work", "Archive"} {
		if err := client.Create(name, nil).Wait(); err != nil {
			t.Fatalf("Create(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"INBOX", "Work"} {
		if err := client.Subscribe(name).Wait(); err != nil {
			t.Fatalf("Subscribe(%q) = %v", name, err)
		}
	}

	mailboxes, err := client.Lsub("", "*").Collect()
	if err != nil {
		t.Fatalf("Lsub() = %v", err)
	}
	if got, want := mailboxNames(mailboxes), []string{"INBOX", "Work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Lsub() = %v, want %v", got, want)
	}

	// LIST still lists every mailbox: the two do not share their answers.
	mailboxes, err = client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if got, want := mailboxNames(mailboxes), []string{"Archive", "INBOX", "Work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}
}

// A LIST response that arrives while LSUB is in flight is not an answer to
// it: with NOTIFY (RFC 5465 §5.4) a server sends LIST responses at any time,
// and they say nothing about subscriptions. Nor is an LSUB response an
// answer to a LIST.
func TestLsub_KeepsToItsOwnResponses(t *testing.T) {
	clientConn, serverConn := net.Pipe()

	var (
		mu         sync.Mutex
		unilateral []string
	)
	client := imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			List: func(data *imap.ListData) {
				mu.Lock()
				unilateral = append(unilateral, data.Mailbox)
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
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		for _, answer := range []string{
			"* LIST () \"/\" Notified\r\n" +
				"* LSUB () \"/\" INBOX\r\n" +
				"* LSUB (\\Noselect) \"/\" Parent\r\n",
			"* LSUB () \"/\" Stray\r\n" +
				"* LIST () \"/\" INBOX\r\n" +
				"* LIST () \"/\" Parent\r\n" +
				"* LIST () \"/\" Parent/Child\r\n",
		} {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			tag, rest, _ := strings.Cut(line, " ")
			name, _, _ := strings.Cut(rest, " ")
			io.WriteString(serverConn, answer+tag+" OK "+name+" completed\r\n")
		}
	}()

	mailboxes, err := client.Lsub("", "*").Collect()
	if err != nil {
		t.Fatalf("Lsub() = %v", err)
	}
	if got, want := mailboxNames(mailboxes), []string{"INBOX", "Parent"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Lsub() = %v, want %v", got, want)
	}

	mailboxes, err = client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if got, want := mailboxNames(mailboxes), []string{"INBOX", "Parent", "Parent/Child"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"Notified"}; !reflect.DeepEqual(unilateral, want) {
		t.Errorf("unilateral LIST responses = %v, want %v", unilateral, want)
	}
}
