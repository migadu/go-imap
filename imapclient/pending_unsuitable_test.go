package imapclient_test

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// RFC 9051 §11.3: "when not in selected state, clients MUST ignore responses /
// response codes related to message and mailbox status such as FLAGS, EXIST,
// EXPUNGE, and FETCH". A pending FETCH, STORE or EXPUNGE does not make them
// suitable: it can only have been sent with no mailbox selected, so the data
// cannot be about one. The command still ends with its tagged response, and a
// FETCH literal is read through so the stream stays in step.
//
// Issue #156, the gap left by #148, which filters unsolicited data only.
func TestPendingCommandIgnoresUnsuitableData(t *testing.T) {
	for _, tc := range []struct {
		name     string
		greeting string
		login    bool // to authenticate before the selected-state check
	}{
		{"before authentication", "* OK [CAPABILITY IMAP4rev1] ready\r\n", true},
		{"without a selected mailbox", "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			go func() {
				io.WriteString(serverConn, tc.greeting)
				br := bufio.NewReader(serverConn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					tag, cmd, _ := strings.Cut(line, " ")
					var data string
					switch {
					case strings.HasPrefix(cmd, "UID FETCH "):
						// The literal before UID takes the buffering path.
						data = "* 1 FETCH (BODY[] {5}\r\nhello UID 42)\r\n"
					case strings.HasPrefix(cmd, "FETCH "):
						data = "* 1 FETCH (UID 42 BODY[] {5}\r\nhello)\r\n"
					case strings.HasPrefix(cmd, "STORE "):
						data = "* 1 FETCH (FLAGS (\\Seen))\r\n"
					case strings.HasPrefix(cmd, "EXPUNGE"):
						data = "* 2 EXPUNGE\r\n"
					case strings.HasPrefix(cmd, "SELECT "):
						data = "* 3 EXISTS\r\n"
					}
					io.WriteString(serverConn, data+tag+" OK done\r\n")
				}
			}()

			client := imapclient.New(clientConn, nil)
			defer client.Close()

			body := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{{}}}
			fetch := func(t *testing.T, numSet imap.NumSet) []*imapclient.FetchMessageBuffer {
				t.Helper()
				msgs, err := client.Fetch(numSet, body).Collect()
				if err != nil {
					t.Fatalf("Fetch(%v) = %v", numSet, err)
				}
				return msgs
			}
			store := func(t *testing.T) []*imapclient.FetchMessageBuffer {
				t.Helper()
				msgs, err := client.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect()
				if err != nil {
					t.Fatalf("Store() = %v", err)
				}
				return msgs
			}
			expunge := func(t *testing.T) []uint32 {
				t.Helper()
				seqNums, err := client.Expunge().Collect()
				if err != nil {
					t.Fatalf("Expunge() = %v", err)
				}
				return seqNums
			}

			if msgs := fetch(t, imap.SeqSetNum(1)); len(msgs) != 0 {
				t.Errorf("FETCH 1 gave %d messages (first UID %v), want none", len(msgs), msgs[0].UID)
			}
			if msgs := fetch(t, imap.UIDSetNum(42)); len(msgs) != 0 {
				t.Errorf("UID FETCH 42 gave %d messages (first UID %v), want none", len(msgs), msgs[0].UID)
			}
			if msgs := store(t); len(msgs) != 0 {
				t.Errorf("STORE 1 gave %d messages (first flags %v), want none", len(msgs), msgs[0].Flags)
			}
			if seqNums := expunge(t); len(seqNums) != 0 {
				t.Errorf("EXPUNGE gave %v, want none", seqNums)
			}
			if err := client.Noop().Wait(); err != nil {
				t.Fatalf("Noop() after the ignored data = %v", err)
			}

			// Once a mailbox is selected, the same responses are delivered.
			if tc.login {
				if err := client.Login("user", "pass").Wait(); err != nil {
					t.Fatalf("Login() = %v", err)
				}
			}
			if _, err := client.Select("INBOX", nil).Wait(); err != nil {
				t.Fatalf("Select() = %v", err)
			}
			for _, numSet := range []imap.NumSet{imap.SeqSetNum(1), imap.UIDSetNum(42)} {
				msgs := fetch(t, numSet)
				if len(msgs) != 1 || msgs[0].UID != 42 || string(msgs[0].FindBodySection(&imap.FetchItemBodySection{})) != "hello" {
					t.Errorf("selected: Fetch(%v) = %+v, want UID 42 with body hello", numSet, msgs)
				}
			}
			if msgs := store(t); len(msgs) != 1 || len(msgs[0].Flags) != 1 {
				t.Errorf("selected: Store() = %+v, want message 1 with its flag", msgs)
			}
			if seqNums := expunge(t); len(seqNums) != 1 || seqNums[0] != 2 {
				t.Errorf("selected: Expunge() = %v, want [2]", seqNums)
			}
		})
	}
}
