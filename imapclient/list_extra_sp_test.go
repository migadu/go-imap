package imapclient_test

import (
	"testing"

	"github.com/migadu/go-imap"
)

// Some servers put extra SPs inside parenthesized lists. Each of these used to
// fail its command and close the connection, e.g. SELECT with
//
//	in response-data: in flag: imapwire: expected flag, got ")"
func TestListExtraSP(t *testing.T) {
	t.Run("SELECT FLAGS trailing SP", func(t *testing.T) {
		c := newCodeServer(t, "* FLAGS (\\Answered \\Flagged \\Seen )\r\n* 1 EXISTS\r\n")
		c.SetStateForTest(imap.ConnStateAuthenticated)
		data, err := c.Select("INBOX", nil).Wait()
		if err != nil {
			t.Fatalf("Select() = %v", err)
		}
		want := []imap.Flag{imap.FlagAnswered, imap.FlagFlagged, imap.FlagSeen}
		if len(data.Flags) != len(want) {
			t.Fatalf("Flags = %v, want %v", data.Flags, want)
		}
		for i := range want {
			if data.Flags[i] != want[i] {
				t.Errorf("Flags = %v, want %v", data.Flags, want)
				break
			}
		}
		if data.NumMessages != 1 {
			t.Errorf("NumMessages = %v, want 1", data.NumMessages)
		}
	})

	t.Run("LIST leading SP", func(t *testing.T) {
		c := newCodeServer(t, "* LIST ( \\HasNoChildren) \"/\" INBOX\r\n")
		c.SetStateForTest(imap.ConnStateAuthenticated)
		mailboxes, err := c.List("", "*", nil).Collect()
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		if len(mailboxes) != 1 || mailboxes[0].Mailbox != "INBOX" ||
			len(mailboxes[0].Attrs) != 1 || mailboxes[0].Attrs[0] != imap.MailboxAttrHasNoChildren {
			t.Errorf("List() = %+v, want INBOX with \\HasNoChildren", mailboxes)
		}
	})

	// imap.139.com (China Mobile), reported in upstream PR emersion/go-imap#788
	// by noir017.
	t.Run("STATUS trailing SP", func(t *testing.T) {
		c := newCodeServer(t, "* STATUS \"INBOX\" (MESSAGES 11 UIDVALIDITY 1 UNSEEN 7 )\r\n")
		c.SetStateForTest(imap.ConnStateAuthenticated)
		data, err := c.Status("INBOX", &imap.StatusOptions{NumMessages: true, UIDValidity: true, NumUnseen: true}).Wait()
		if err != nil {
			t.Fatalf("Status() = %v", err)
		}
		if data.NumMessages == nil || *data.NumMessages != 11 || data.UIDValidity != 1 ||
			data.NumUnseen == nil || *data.NumUnseen != 7 {
			t.Errorf("Status() = %+v, want MESSAGES 11 UIDVALIDITY 1 UNSEEN 7", data)
		}
	})
}
