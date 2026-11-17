package imapclient_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestFetchSearchResult(t *testing.T) {
	for _, uidSearch := range []bool{false, true} {
		for _, returnAll := range []bool{false, true} {
			t.Run(fmt.Sprintf("UIDSearch=%v/ALL=%v", uidSearch, returnAll), func(t *testing.T) {
				conn, server := newMemClientServerPair(t)
				defer server.Close()
				c := imapclient.New(conn, nil)
				defer c.Close()
				if err := c.Login(testUsername, testPassword).Wait(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Enable(imap.CapIMAP4rev2).Wait(); err != nil {
					t.Fatal(err)
				}
				for _, group := range []string{"discard", "target", "other", "target", "other", "target"} {
					raw := "X-Saved-Set: " + group + "\r\n\r\nbody\r\n"
					cmd := c.Append("INBOX", int64(len(raw)), nil)
					if _, err := cmd.Write([]byte(raw)); err != nil {
						t.Fatal(err)
					}
					if err := cmd.Close(); err != nil {
						t.Fatal(err)
					}
					if _, err := cmd.Wait(); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					t.Fatal(err)
				}
				if err := c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
					t.Fatal(err)
				}
				if err := c.UIDExpunge(imap.UIDSetNum(1)).Close(); err != nil {
					t.Fatal(err)
				}

				save := func(group string) *imapclient.SearchCommand {
					criteria := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Saved-Set", Value: group, IsHeader: true}}}
					options := &imap.SearchOptions{ReturnSave: true, ReturnAll: returnAll}
					if uidSearch {
						return c.UIDSearch(criteria, options)
					}
					return c.Search(criteria, options)
				}
				checkMessages := func(messages []*imapclient.FetchMessageBuffer, want []imap.UID, body *imap.FetchItemBodySection, flagged bool) {
					t.Helper()
					var got []imap.UID
					for _, msg := range messages {
						got = append(got, msg.UID)
						// Expunging UID 1 makes the UID differ from its sequence number.
						if msg.SeqNum != uint32(msg.UID)-1 {
							t.Errorf("UID %d has sequence number %d, want %d", msg.UID, msg.SeqNum, msg.UID-1)
						}
						if body != nil && string(msg.FindBodySection(body)) != "body\r\n" {
							t.Errorf("UID %d body = %q", msg.UID, msg.FindBodySection(body))
						}
						if flagged && !slices.Contains(msg.Flags, imap.FlagFlagged) {
							t.Errorf("UID %d STORE response lacks \\Flagged: %v", msg.UID, msg.Flags)
						}
					}
					if !slices.Equal(got, want) {
						t.Errorf("returned UIDs = %v, want %v", got, want)
					}
				}
				body := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true}
				for _, tc := range []struct {
					group string
					want  []imap.UID
				}{
					{"target", []imap.UID{2, 4, 6}},
					{"other", []imap.UID{3, 5}},
					{"no-match", nil},
				} {
					// SEARCHRES permits a SAVE and its dependent FETCH to be pipelined,
					// including SAVE-only where no result set reaches the client.
					search := save(tc.group)
					fetch := c.Fetch(imap.SearchRes(), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{body}})
					if _, err := search.Wait(); err != nil {
						t.Fatal(err)
					}
					messages, err := fetch.Collect()
					if err != nil {
						t.Fatal(err)
					}
					checkMessages(messages, tc.want, body, false)
					// Non-silent STORE uses the same FETCH result path.
					messages, err = c.Store(imap.SearchRes(), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect()
					if err != nil {
						t.Fatal(err)
					}
					checkMessages(messages, tc.want, nil, true)
				}
				if err := c.Noop().Wait(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
