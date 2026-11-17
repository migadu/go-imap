package imapclient_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestSearchSaveUpdatesSessionResult(t *testing.T) {
	for _, api := range []string{"SEARCH", "UID SEARCH", "ESEARCH selected"} {
		for _, returnAll := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ALL=%v", api, returnAll), func(t *testing.T) {
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
				for _, group := range []string{"discard", "other", "target", "other", "target", "other"} {
					raw := "X-Save-Probe: " + group + "\r\n\r\nbody\r\n"
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

				search := func(group string, save, all bool) *imap.SearchData {
					t.Helper()
					criteria := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "X-Save-Probe", Value: group, IsHeader: true}}}
					options := &imap.SearchOptions{ReturnSave: save, ReturnAll: all}
					var data *imap.SearchData
					var err error
					switch api {
					case "SEARCH":
						data, err = c.Search(criteria, options).Wait()
					case "UID SEARCH":
						data, err = c.UIDSearch(criteria, options).Wait()
					default:
						var results []*imap.SearchData
						results, err = c.MultiSearch(&imap.SearchSource{Selected: true}, criteria, options).Wait()
						data = &imap.SearchData{}
						if len(results) == 1 {
							data = results[0]
						} else if len(results) != 0 {
							t.Fatalf("selected-source ESEARCH returned %d mailboxes", len(results))
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
				checkSaved := func(want ...imap.UID) {
					t.Helper()
					data, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{imap.SearchRes()}}, &imap.SearchOptions{ReturnAll: true}).Wait()
					if err != nil {
						t.Fatal(err)
					}
					got := data.AllUIDs()
					if !slices.Equal(got, want) {
						t.Fatalf("UID SEARCH UID $ = %v, want %v", got, want)
					}
				}

				search("other", true, true)
				checkSaved(2, 4, 6)
				data := search("target", true, returnAll)
				if returnAll {
					want := "3,5"
					if api == "SEARCH" {
						want = "2,4"
					}
					if data.All == nil || data.All.String() != want {
						t.Errorf("search ALL = %v, want %s", data.All, want)
					}
				} else if data.All != nil && data.All.String() != "" {
					t.Errorf("SAVE-only search returned results: %v", data.All)
				}
				checkSaved(3, 5)
				if err := c.Store(imap.SearchRes(), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close(); err != nil {
					t.Fatal(err)
				}
				messages, err := c.Fetch(imap.UIDSetNum(2, 3, 4, 5, 6), &imap.FetchOptions{UID: true, Flags: true}).Collect()
				if err != nil {
					t.Fatal(err)
				}
				if len(messages) != 5 {
					t.Fatalf("FETCH after STORE $ returned %d messages, want 5", len(messages))
				}
				for _, msg := range messages {
					if flagged := slices.Contains(msg.Flags, imap.FlagFlagged); flagged != (msg.UID == 3 || msg.UID == 5) {
						t.Errorf("STORE $ changed wrong message: UID=%d flags=%v", msg.UID, msg.Flags)
					}
				}
				search("other", false, true)
				checkSaved(3, 5)
				search("no-match", true, returnAll)
				checkSaved()
			})
		}
	}
}
