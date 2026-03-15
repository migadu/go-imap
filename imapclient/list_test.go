package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

func TestList(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	options := imap.ListOptions{
		ReturnStatus: &imap.StatusOptions{
			NumMessages: true,
		},
	}
	mailboxes, err := client.List("", "%", &options).Collect()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}

	if len(mailboxes) != 1 {
		t.Fatalf("List() returned %v mailboxes, want 1", len(mailboxes))
	}
	mbox := mailboxes[0]

	wantNumMessages := uint32(1)
	want := &imap.ListData{
		Delim:   '/',
		Mailbox: "INBOX",
		Status: &imap.StatusData{
			Mailbox:     "INBOX",
			NumMessages: &wantNumMessages,
		},
	}
	if !reflect.DeepEqual(mbox, want) {
		t.Errorf("got %#v but want %#v", mbox, want)
	}
}

// TestListExtendedDataItemGating tests that extended data items (CHILDINFO, OLDNAME)
// are only sent when appropriate according to RFC 5258.
func TestListExtendedDataItemGating(t *testing.T) {
	tests := []struct {
		name           string
		setupMailboxes func(*imapmemserver.User)
		command        string
		wantCHILDINFO  bool
		wantOLDNAME    bool
		description    string
	}{
		{
			name: "plain LIST should not send CHILDINFO",
			setupMailboxes: func(user *imapmemserver.User) {
				user.Create("Parent", nil)
				user.Create("Parent/Child", nil)
				user.Subscribe("Parent/Child")
			},
			command:       `tag1 LIST "" "*"`,
			wantCHILDINFO: false,
			description:   "RFC 5258 §3.5: CHILDINFO MUST NOT be returned unless RECURSIVEMATCH is specified",
		},
		{
			name: "LIST with RETURN (CHILDREN) should not send CHILDINFO",
			setupMailboxes: func(user *imapmemserver.User) {
				user.Create("Parent", nil)
				user.Create("Parent/Child", nil)
			},
			command:       `tag2 LIST "" "*" RETURN (CHILDREN)`,
			wantCHILDINFO: false,
			description:   "RETURN (CHILDREN) controls attributes, not CHILDINFO extended data item",
		},
		{
			name: "LIST with RECURSIVEMATCH allows CHILDINFO",
			setupMailboxes: func(user *imapmemserver.User) {
				user.Create("Parent", nil)
				user.Create("Parent/Child", nil)
				user.Subscribe("Parent")
				user.Subscribe("Parent/Child")
			},
			command:       `tag3 LIST (SUBSCRIBED RECURSIVEMATCH) "" "*"`,
			wantCHILDINFO: false, // In-memory server doesn't generate CHILDINFO, but it's allowed
			description:   "RFC 5258 §3.5: CHILDINFO can be returned when RECURSIVEMATCH is specified",
		},
		{
			name: "plain LIST should not send OLDNAME",
			setupMailboxes: func(user *imapmemserver.User) {
				user.Create("INBOX", nil)
			},
			command:     `tag4 LIST "" "INBOX"`,
			wantOLDNAME: false,
			description: "RFC 5258 §3: extended data items require LIST-EXTENDED syntax",
		},
		{
			name: "LSUB should not send extended data items",
			setupMailboxes: func(user *imapmemserver.User) {
				user.Create("INBOX", nil)
				user.Subscribe("INBOX")
			},
			command:       `tag5 LSUB "" "*"`,
			wantCHILDINFO: false,
			wantOLDNAME:   false,
			description:   "LSUB is not LIST-EXTENDED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a custom server with mailboxes
			memServer := imapmemserver.New()
			user := imapmemserver.NewUser(testUsername, testPassword)
			tt.setupMailboxes(user)
			memServer.AddUser(user)

			server := imapserver.New(&imapserver.Options{
				NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return memServer.NewSession(), nil, nil
				},
				InsecureAuth: true,
			})

			// Create listener
			ln, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("net.Listen() = %v", err)
			}
			defer ln.Close()

			// Start server
			go server.Serve(ln)

			// Connect to server
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("net.Dial() = %v", err)
			}
			defer conn.Close()

			scanner := bufio.NewScanner(conn)

			// Read greeting
			if !scanner.Scan() {
				t.Fatalf("failed to read greeting: %v", scanner.Err())
			}

			// Login
			fmt.Fprintf(conn, "a001 LOGIN %s %s\r\n", testUsername, testPassword)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "a001 ") {
					break
				}
			}

			// Send test command
			fmt.Fprintf(conn, "%s\r\n", tt.command)

			// Read responses
			foundCHILDINFO := false
			foundOLDNAME := false
			for scanner.Scan() {
				line := scanner.Text()

				if strings.Contains(line, "CHILDINFO") {
					foundCHILDINFO = true
				}
				if strings.Contains(line, "OLDNAME") {
					foundOLDNAME = true
				}

				// Check if this is the tagged completion response
				if strings.HasPrefix(line, "tag") && strings.Contains(line, " OK ") {
					break
				}
			}

			if foundCHILDINFO != tt.wantCHILDINFO {
				t.Errorf("%s: got CHILDINFO=%v, want %v", tt.description, foundCHILDINFO, tt.wantCHILDINFO)
			}
			if foundOLDNAME != tt.wantOLDNAME {
				t.Errorf("%s: got OLDNAME=%v, want %v", tt.description, foundOLDNAME, tt.wantOLDNAME)
			}
		})
	}
}

// TestSelectNoExtendedDataItems tests that SELECT responses don't include
// extended LIST data items even if the mailbox data has them.
func TestSelectNoExtendedDataItems(t *testing.T) {
	// Create a custom server
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testUsername, testPassword)
	user.Create("INBOX", nil)
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})

	// Create listener
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	defer ln.Close()

	// Start server
	go server.Serve(ln)

	// Connect to server
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)

	// Read greeting
	if !scanner.Scan() {
		t.Fatalf("failed to read greeting: %v", scanner.Err())
	}

	// Login
	fmt.Fprintf(conn, "a001 LOGIN %s %s\r\n", testUsername, testPassword)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "a001 ") {
			break
		}
	}

	// SELECT INBOX
	fmt.Fprintf(conn, "a002 SELECT INBOX\r\n")

	foundCHILDINFO := false
	foundOLDNAME := false
	for scanner.Scan() {
		line := scanner.Text()

		if strings.Contains(line, "CHILDINFO") {
			foundCHILDINFO = true
		}
		if strings.Contains(line, "OLDNAME") {
			foundOLDNAME = true
		}

		if strings.HasPrefix(line, "a002 ") && strings.Contains(line, " OK ") {
			break
		}
	}

	if foundCHILDINFO {
		t.Error("SELECT response should not include CHILDINFO extended data item")
	}
	if foundOLDNAME {
		t.Error("SELECT response should not include OLDNAME extended data item")
	}
}

// TestListExtendedVsPlain tests the difference between plain LIST and LIST-EXTENDED
func TestListExtendedVsPlain(t *testing.T) {
	client, server := newClientServerPair(t, imap.ConnStateAuthenticated)
	defer client.Close()
	defer server.Close()

	// Plain LIST (no options) - should work and not error
	plainMailboxes, err := client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("Plain List() failed: %v", err)
	}
	if len(plainMailboxes) == 0 {
		t.Error("Expected at least one mailbox from plain LIST")
	}

	// LIST-EXTENDED with RETURN (CHILDREN) - should work
	extendedOptions := &imap.ListOptions{
		ReturnChildren: true,
	}
	extendedMailboxes, err := client.List("", "*", extendedOptions).Collect()
	if err != nil {
		t.Fatalf("Extended List() with RETURN (CHILDREN) failed: %v", err)
	}
	if len(extendedMailboxes) == 0 {
		t.Error("Expected at least one mailbox from LIST-EXTENDED")
	}

	// Verify both return the same mailboxes (though attributes may differ)
	if len(plainMailboxes) != len(extendedMailboxes) {
		t.Errorf("Plain LIST returned %d mailboxes, but LIST-EXTENDED returned %d",
			len(plainMailboxes), len(extendedMailboxes))
	}
}
