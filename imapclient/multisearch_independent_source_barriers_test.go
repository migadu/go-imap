package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/migadu/go-imap"
)

type multiSearchBarrierSource struct {
	name     string
	source   *imap.SearchSource
	criteria *imap.SearchCriteria
	mailbox  string
}

func independentMultiSearchBarrierSources() []multiSearchBarrierSource {
	return []multiSearchBarrierSource{
		{name: "mailboxes", source: &imap.SearchSource{Mailboxes: []string{"Archive"}}, mailbox: "Archive"},
		{name: "inboxes", source: &imap.SearchSource{Inboxes: true}, mailbox: "INBOX"},
		{name: "personal", source: &imap.SearchSource{Personal: true}, mailbox: "Archive"},
		{name: "subscribed", source: &imap.SearchSource{Subscribed: true}, mailbox: "Archive"},
		{name: "subtree", source: &imap.SearchSource{Subtree: []string{"Archive"}}, mailbox: "Archive"},
		{name: "subtree-one", source: &imap.SearchSource{SubtreeOne: []string{"Archive"}}, mailbox: "Archive"},
		{
			name:     "mailboxes-modseq",
			source:   &imap.SearchSource{Mailboxes: []string{"Archive"}},
			criteria: &imap.SearchCriteria{ModSeq: &imap.SearchCriteriaModSeq{ModSeq: 1}},
			mailbox:  "Archive",
		},
	}
}

// RFC 7377 sections 2.1 and 2.2 allow these sources in authenticated state;
// they do not depend on which mailbox SELECT/UNSELECT/CLOSE leaves selected.
// Their tagged ESEARCH correlators remain meaningful while a preceding
// selection transition is pending, even if the scope can include the old
// selected mailbox or the criteria contain MODSEQ.
func TestMultiSearchIndependentSourcesAcrossSelectionBarrier(t *testing.T) {
	for _, barrier := range []string{"SELECT", "UNSELECT", "CLOSE"} {
		for _, source := range independentMultiSearchBarrierSources() {
			t.Run(barrier+"/"+source.name, func(t *testing.T) {
				testMultiSearchSourceAcrossBarrier(t, barrier, source, true)
			})
		}
	}
}

func TestMultiSearchSelectedSourcesDoNotCrossSelectionBarrier(t *testing.T) {
	for _, barrier := range []string{"SELECT", "UNSELECT", "CLOSE"} {
		for _, source := range []multiSearchBarrierSource{
			{name: "nil", mailbox: "Old"},
			{name: "zero", source: &imap.SearchSource{}, mailbox: "Old"},
			{name: "selected", source: &imap.SearchSource{Selected: true}, mailbox: "Old"},
			{name: "selected-and-mailboxes", source: &imap.SearchSource{Selected: true, Mailboxes: []string{"Archive"}}, mailbox: "Old"},
		} {
			t.Run(barrier+"/"+source.name, func(t *testing.T) {
				testMultiSearchSourceAcrossBarrier(t, barrier, source, false)
			})
		}
	}
}

func TestMultiSearchIndependentSourcesDoNotCrossUnauthenticateBarrier(t *testing.T) {
	for _, source := range independentMultiSearchBarrierSources() {
		t.Run(source.name, func(t *testing.T) {
			testMultiSearchSourceAcrossBarrier(t, "UNAUTHENTICATE", source, false)
		})
	}
}

func testMultiSearchSourceAcrossBarrier(t *testing.T, barrier string, source multiSearchBarrierSource, wantResult bool) {
	t.Helper()
	c, done := newSelectIsolation307Peer(t, "IMAP4rev2 CONDSTORE MULTISEARCH UNAUTHENTICATE", nil, func(br *bufio.Reader, conn net.Conn) error {
		initialTag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if err := writeSelectIsolation307Data(conn, initialTag, 3, `\Seen`, `\Seen`); err != nil {
			return err
		}
		barrierTag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		searchTag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}

		// Independent scopes can validly return data before the selection
		// transition completes. For selected/authentication controls, send
		// the same premature response and verify that it is discarded.
		modSeq := ""
		if source.criteria != nil && source.criteria.ModSeq != nil {
			modSeq = " MODSEQ 43"
		}
		if _, err := fmt.Fprintf(conn, "* ESEARCH (TAG %q MAILBOX %q UIDVALIDITY 7) UID ALL 31%s\r\n", searchTag, source.mailbox, modSeq); err != nil {
			return err
		}

		if barrier == "SELECT" {
			if _, err := fmt.Fprint(conn, "* OK [CLOSED] old selection ended\r\n"); err != nil {
				return err
			}
			if err := writeSelectIsolation307Data(conn, barrierTag, 2, `\Flagged`, `\Flagged`); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(conn, "%s OK %s completed\r\n", barrierTag, barrier); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(conn, "%s OK ESEARCH completed\r\n", searchTag); err != nil {
			return err
		}

		if barrier == "UNAUTHENTICATE" {
			// UNAUTHENTICATE invalidates the capability cache. No further
			// command is needed to observe the completed result below.
			return nil
		}
		return finishSelectIsolation307Noop(br, conn)
	})

	if _, err := c.Select("Old", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	var waitBarrier func() error
	switch barrier {
	case "SELECT":
		cmd := c.Select("New", nil)
		waitBarrier = func() error { _, err := cmd.Wait(); return err }
	case "UNSELECT":
		waitBarrier = c.Unselect().Wait
	case "CLOSE":
		waitBarrier = c.UnselectAndExpunge().Wait
	case "UNAUTHENTICATE":
		waitBarrier = c.Unauthenticate().Wait
	default:
		t.Fatalf("unknown barrier %q", barrier)
	}
	criteria := source.criteria
	if criteria == nil {
		criteria = &imap.SearchCriteria{}
	}
	cmd := c.MultiSearch(source.source, criteria, &imap.SearchOptions{ReturnAll: true})
	if err := waitBarrier(); err != nil {
		t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if barrier != "UNAUTHENTICATE" {
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if !wantResult {
		if len(data) != 0 {
			t.Fatalf("premature ESEARCH result crossed %s barrier: %+v", barrier, data)
		}
		return
	}
	if len(data) != 1 {
		t.Fatalf("independent ESEARCH result lost behind %s: got %d results, want 1", barrier, len(data))
	}
	if data[0].Mailbox != source.mailbox || data[0].UIDValidity != 7 ||
		!reflect.DeepEqual(data[0].AllUIDs(), []imap.UID{31}) {
		t.Fatalf("independent ESEARCH result corrupted: %+v", data[0])
	}
	if source.criteria != nil && source.criteria.ModSeq != nil && data[0].ModSeq != 43 {
		t.Fatalf("independent ESEARCH result lost MODSEQ: %+v", data[0])
	}
	if mbox := c.Mailbox(); barrier == "SELECT" {
		if mbox == nil || mbox.Name != "New" || mbox.NumMessages != 2 {
			t.Fatalf("selection transition corrupted: %+v", mbox)
		}
	} else if mbox != nil {
		t.Fatalf("%s left a mailbox selected: %+v", barrier, mbox)
	}
}
