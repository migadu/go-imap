package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// handlerLog records which unilateral data handlers were called.
type handlerLog struct {
	mutex sync.Mutex
	calls []string
}

func (l *handlerLog) note(name string) {
	l.mutex.Lock()
	l.calls = append(l.calls, name)
	l.mutex.Unlock()
}

func (l *handlerLog) take() []string {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	calls := l.calls
	l.calls = nil
	slices.Sort(calls)
	return calls
}

// mailboxData is what a server has to say about a selected mailbox and its
// messages, and about other mailboxes.
const mailboxData = "* FLAGS (\\Seen)\r\n" +
	"* 3 EXISTS\r\n" +
	"* 2 EXPUNGE\r\n" +
	"* 1 FETCH (FLAGS (\\Seen))\r\n" +
	"* VANISHED 7\r\n" +
	"* LIST () \"/\" Other\r\n" +
	"* STATUS Other (MESSAGES 1)\r\n"

// RFC 9051 §11.3: "Before authentication, clients SHOULD ignore any responses
// other than CAPABILITY and server status responses [...]. Unless explicitly
// allowed by an IMAP extension, when not in selected state, clients MUST
// ignore responses / response codes related to message and mailbox status such
// as FLAGS, EXIST, EXPUNGE, and FETCH."
//
// Upstream: an open item of emersion/go-imap#491. Issue #147.
func TestResponsesUnsuitableForState(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		defer serverConn.Close()
		br := bufio.NewReader(serverConn)
		// Injected ahead of authentication, as §11.3 has it.
		io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1 QRESYNC] ready\r\n"+mailboxData)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			switch fields[1] {
			case "NOOP":
				// The same data again, in whatever state the client is.
				io.WriteString(serverConn, mailboxData+fields[0]+" OK NOOP completed\r\n")
			case "SELECT":
				io.WriteString(serverConn, "* 3 EXISTS\r\n* FLAGS (\\Seen)\r\n"+fields[0]+" OK [READ-WRITE] SELECT completed\r\n")
			default:
				io.WriteString(serverConn, fields[0]+" OK completed\r\n")
			}
		}
	}()

	log := &handlerLog{}
	client := imapclient.New(clientConn, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Expunge:  func(uint32) { log.note("Expunge") },
			Mailbox:  func(*imapclient.UnilateralDataMailbox) { log.note("Mailbox") },
			Fetch:    func(*imapclient.FetchMessageData) { log.note("Fetch") },
			List:     func(*imap.ListData) { log.note("List") },
			Status:   func(*imap.StatusData) { log.note("Status") },
			Vanished: func(*imap.VanishedData) { log.note("Vanished") },
		},
	})
	defer client.Close()

	// noop has the server send the data, and returns the handlers called.
	// The Fetch handler runs on its own goroutine: give it time.
	noop := func() []string {
		t.Helper()
		if err := client.Noop().Wait(); err != nil {
			t.Fatalf("Noop() = %v", err)
		}
		time.Sleep(100 * time.Millisecond)
		return log.take()
	}

	// Not authenticated: the data after the greeting, and again after NOOP.
	if calls := noop(); len(calls) != 0 {
		t.Errorf("before authentication the handlers called were %v, want none", calls)
	}

	// Authenticated, nothing selected: no mailbox to take the message data;
	// LIST and STATUS tell of other mailboxes (NOTIFY, RFC 5465) and stay.
	if err := client.Login("user", "pass").Wait(); err != nil {
		t.Fatalf("Login() = %v", err)
	}
	if calls, want := noop(), []string{"List", "Status"}; !slices.Equal(calls, want) {
		t.Errorf("with no mailbox selected the handlers called were %v, want %v", calls, want)
	}

	// The responses to SELECT come before the state changes, and count.
	data, err := client.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}
	if data.NumMessages != 3 || len(data.Flags) != 1 {
		t.Errorf("Select() = %+v, want the 3 messages and 1 flag of its responses", data)
	}
	log.take()

	// Selected: everything is delivered.
	want := []string{"Expunge", "Fetch", "List", "Mailbox", "Mailbox", "Status", "Vanished"}
	if calls := noop(); !slices.Equal(calls, want) {
		t.Errorf("in the selected state the handlers called were %v, want %v", calls, want)
	}

	// Unselected again: as before SELECT, and the mailbox is gone for good.
	if err := client.Unselect().Wait(); err != nil {
		t.Fatalf("Unselect() = %v", err)
	}
	if calls, want := noop(), []string{"List", "Status"}; !slices.Equal(calls, want) {
		t.Errorf("after UNSELECT the handlers called were %v, want %v", calls, want)
	}
	if mbox := client.Mailbox(); mbox != nil {
		t.Errorf("Mailbox() = %+v after UNSELECT, want nil", mbox)
	}
}
