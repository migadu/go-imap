package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

// STATUS Archive and SELECT Other are independent, so RFC 9051 section 5.5
// allows the server to complete SELECT first. Its opening responses belong to
// SELECT even while STATUS is the oldest pending command. With an old mailbox
// selected, CLOSED also separates its updates from the new opening data
// (RFC 7162 section 3.2.11).
func TestSelectHighestModSeqAfterIndependentStatus(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(fmt.Sprintf("previously_selected=%v", selected), func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			serverErr := make(chan error, 1)
			go func() { serverErr <- serveSelectBeforeStatus(serverConn, selected) }()

			c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 2 * time.Second})
			t.Cleanup(func() {
				c.Close()
				serverConn.Close()
				if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
					t.Errorf("test server: %v", err)
				}
			})
			if selected {
				if _, err := c.Select("INBOX", &imap.SelectOptions{CondStore: true}).Wait(); err != nil {
					t.Fatal(err)
				}
			}

			statusCmd := c.Status("Archive", &imap.StatusOptions{NumMessages: true})
			selectCmd := c.Select("Other", &imap.SelectOptions{CondStore: true})
			data, err := selectCmd.Wait()
			if err != nil {
				t.Fatalf("Select.Wait: %v", err)
			}
			if data.HighestModSeq != 77 {
				t.Errorf("Select(Other).HighestModSeq = %d, want 77", data.HighestModSeq)
			}
			if len(data.Modified) != 0 {
				t.Errorf("Select(Other).Modified = %v, want no updates from the closing mailbox", data.Modified)
			}
			status, err := statusCmd.Wait()
			if err != nil {
				t.Fatalf("Status.Wait: %v", err)
			}
			if status.NumMessages == nil || *status.NumMessages != 4 {
				t.Errorf("Status(Archive).NumMessages = %v, want 4", status.NumMessages)
			}
		})
	}
}

func serveSelectBeforeStatus(conn net.Conn, selected bool) error {
	defer conn.Close()
	br := bufio.NewReader(conn)
	if _, err := fmt.Fprint(conn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] ready\r\n"); err != nil {
		return err
	}
	if selected {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		tag, _, _ := strings.Cut(line, " ")
		if _, err := fmt.Fprintf(conn,
			"* 1 EXISTS\r\n"+
				"* OK [UIDVALIDITY 1] ok\r\n"+
				"* OK [UIDNEXT 2] ok\r\n"+
				"* FLAGS (\\Seen)\r\n"+
				"* OK [PERMANENTFLAGS (\\Seen)] ok\r\n"+
				"* OK [HIGHESTMODSEQ 10] ok\r\n"+
				"%s OK [READ-WRITE] selected\r\n", tag); err != nil {
			return err
		}
	}

	statusLine, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	selectLine, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	statusFields, selectFields := strings.Fields(statusLine), strings.Fields(selectLine)
	if len(statusFields) != 4 || statusFields[1] != "STATUS" || strings.Trim(statusFields[2], `"`) != "Archive" ||
		len(selectFields) != 4 || selectFields[1] != "SELECT" || strings.Trim(selectFields[2], `"`) != "Other" {
		return fmt.Errorf("unexpected pipelined commands %q / %q", statusLine, selectLine)
	}
	if selected {
		if _, err := fmt.Fprint(conn,
			"* 1 FETCH (UID 1 FLAGS (\\Seen) MODSEQ (12))\r\n"+
				"* OK [HIGHESTMODSEQ 12] closing watermark\r\n"+
				"* OK [CLOSED] old mailbox closed\r\n"); err != nil {
			return err
		}
	}
	// Complete the later SELECT before supplying the independent STATUS result.
	if _, err := fmt.Fprintf(conn,
		"* 2 EXISTS\r\n"+
			"* OK [UIDVALIDITY 2] ok\r\n"+
			"* OK [UIDNEXT 3] ok\r\n"+
			"* FLAGS (\\Seen)\r\n"+
			"* OK [PERMANENTFLAGS (\\Seen)] ok\r\n"+
			"* OK [HIGHESTMODSEQ 77] new mailbox\r\n"+
			"%s OK [READ-WRITE] selected\r\n"+
			"* STATUS Archive (MESSAGES 4)\r\n"+
			"%s OK status completed\r\n", selectFields[0], statusFields[0]); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, br)
	return err
}
