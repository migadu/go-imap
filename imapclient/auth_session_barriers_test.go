package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func newUnauthBarrierPeer(t *testing.T, caps string, opts *imapclient.Options, serve func(*bufio.Reader, net.Conn) error) (*imapclient.Client, <-chan error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		if _, err := fmt.Fprintf(serverConn, "* OK [CAPABILITY %s] ready\r\n", caps); err != nil {
			done <- err
			return
		}
		done <- serve(bufio.NewReader(serverConn), serverConn)
	}()
	if opts == nil {
		opts = &imapclient.Options{ResponseTimeout: 2 * time.Second}
	} else if opts.ResponseTimeout == 0 {
		opts.ResponseTimeout = 2 * time.Second
	}
	c := imapclient.New(clientConn, opts)
	t.Cleanup(func() { c.Close(); serverConn.Close() })
	return c, done
}

// RFC 9051 §5.5 and §11.3, RFC 7377 §2.2: a pipelined LOGIN is an
// authentication barrier just like UNAUTHENTICATE. Even when an independent
// MultiSearch (e.g. IN (personal)) is pipelined after LOGIN, after LOGIN +
// SELECT, or after UNAUTHENTICATE + LOGIN, an untagged ESEARCH emitted before
// LOGIN's tagged OK arrives in Not Authenticated state and must not cross the
// pending LOGIN barrier.
func TestPipelinedLoginBarrierBlocksPreAuthMultiSearch(t *testing.T) {
	for _, mode := range []string{"login_only", "login_then_select", "unauth_then_login"} {
		t.Run(mode, func(t *testing.T) {
			const caps = "IMAP4rev2 MULTISEARCH UNAUTHENTICATE"
			serve := func(br *bufio.Reader, conn net.Conn) error {
				if mode == "unauth_then_login" {
					unauthTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", unauthTag, caps); err != nil {
						return err
					}
				}

				loginTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				var selTag string
				if mode == "login_then_select" {
					selTag, err = readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
				}
				msTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}

				if _, err := fmt.Fprintf(conn, "* ESEARCH (TAG %q MAILBOX \"INBOX\" UIDVALIDITY 7) UID ALL 31\r\n", msTag); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in\r\n", loginTag, caps); err != nil {
					return err
				}
				if mode == "login_then_select" {
					if err := writeSelectIsolation307Data(conn, selTag, 0, `\Seen`, `\Seen`); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintf(conn, "%s OK ESEARCH completed\r\n", msTag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			}

			var (
				c    *imapclient.Client
				done <-chan error
			)
			if mode == "unauth_then_login" {
				c, done = newSelectIsolation307Peer(t, caps, nil, serve)
				if err := c.Unauthenticate().Wait(); err != nil {
					t.Fatal(err)
				}
			} else {
				c, done = newUnauthBarrierPeer(t, caps, nil, serve)
				if err := c.WaitGreeting(); err != nil {
					t.Fatal(err)
				}
			}

			loginCmd := c.Login("alice", "secret")
			var selCmd *imapclient.SelectCommand
			if mode == "login_then_select" {
				selCmd = c.Select("INBOX", nil)
			}
			msCmd := c.MultiSearch(
				&imap.SearchSource{Personal: true},
				&imap.SearchCriteria{},
				&imap.SearchOptions{ReturnAll: true},
			)

			if err := loginCmd.Wait(); err != nil {
				t.Fatal(err)
			}
			if selCmd != nil {
				if _, err := selCmd.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			data, err := msCmd.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			if len(data) != 0 {
				t.Fatalf("pre-LOGIN unauthenticated ESEARCH crossed pending LOGIN barrier into MultiSearch: got %+v, want empty", data[0])
			}
		})
	}
}

// Pending LOGIN and LOGOUT commands are authentication/session barriers and
// must stop MultiSearch, SEARCH, ESEARCH, SORT, ESORT, THREAD, native MOVE
// COPYUID, FETCH, and EXPUNGE attribution across the boundary.
func TestPipelinedLoginAndLogoutBarriersBlockSiblings(t *testing.T) {
	for _, barrier := range []string{"LOGIN", "LOGOUT"} {
		for _, kind := range []string{
			"MultiSearch_independent",
			"UIDSearch_plain",
			"UIDSearch_ESEARCH",
			"UIDSort_plain",
			"UIDSort_ESORT",
			"UIDSort_ESEARCH_reverse",
			"UIDThread",
			"UIDMove_COPYUID",
			"UIDFetch",
			"Expunge",
		} {
			if barrier == "LOGIN" && (kind == "UIDFetch" || kind == "Expunge") {
				// Pre-auth FETCH/EXPUNGE are already dropped by ignoresMailboxData;
				// LOGOUT in selected state exercises the pending-command barrier.
				continue
			}
			t.Run(barrier+"/"+kind, func(t *testing.T) {
				const caps = "IMAP4rev2 SORT ESORT THREAD=REFERENCES MOVE UIDPLUS MULTISEARCH"
				serve := func(br *bufio.Reader, conn net.Conn) error {
					if barrier == "LOGOUT" {
						selTag, err := readSelectIsolation307Command(br)
						if err != nil {
							return err
						}
						if err := writeSelectIsolation307Data(conn, selTag, 3, `\Seen`, `\Seen`); err != nil {
							return err
						}
					}

					barrierTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}
					cmdTag, err := readSelectIsolation307Command(br)
					if err != nil {
						return err
					}

					var pre string
					switch kind {
					case "MultiSearch_independent":
						pre = fmt.Sprintf("* ESEARCH (TAG %q MAILBOX \"Archive\" UIDVALIDITY 7) UID ALL 99\r\n", cmdTag)
					case "UIDSearch_plain":
						pre = "* SEARCH 99\r\n"
					case "UIDSearch_ESEARCH":
						pre = fmt.Sprintf("* ESEARCH (TAG %q) UID ALL 99\r\n", cmdTag)
					case "UIDSort_plain":
						pre = "* SORT 99\r\n"
					case "UIDSort_ESORT":
						pre = "* ESORT UID ALL 99,1\r\n"
					case "UIDSort_ESEARCH_reverse":
						pre = fmt.Sprintf("* ESEARCH (TAG %q) UID ALL 99,1\r\n", cmdTag)
					case "UIDThread":
						pre = "* THREAD (99)\r\n"
					case "UIDMove_COPYUID":
						pre = "* OK [COPYUID 10 1 99] prior move\r\n"
					case "UIDFetch":
						pre = "* 1 FETCH (UID 99 FLAGS (\\Seen))\r\n"
					case "Expunge":
						pre = "* 1 EXPUNGE\r\n"
					}
					if _, err := fmt.Fprint(conn, pre); err != nil {
						return err
					}

					if barrier == "LOGIN" {
						if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in\r\n%s OK completed\r\n", barrierTag, caps, cmdTag); err != nil {
							return err
						}
					} else {
						if _, err := fmt.Fprintf(conn, "* BYE logging out\r\n%s OK logout\r\n%s OK completed\r\n", barrierTag, cmdTag); err != nil {
							return err
						}
					}
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return nil
						}
						tTag, _, _ := strings.Cut(strings.TrimSpace(line), " ")
						if _, err := fmt.Fprintf(conn, "%s OK done\r\n", tTag); err != nil {
							return err
						}
					}
				}

				var (
					c    *imapclient.Client
					done <-chan error
				)
				if barrier == "LOGIN" {
					c, done = newUnauthBarrierPeer(t, caps, nil, serve)
					if err := c.WaitGreeting(); err != nil {
						t.Fatal(err)
					}
				} else {
					c, done = newSelectIsolation307Peer(t, caps, nil, serve)
					if _, err := c.Select("Work", nil).Wait(); err != nil {
						t.Fatal(err)
					}
				}

				var barrierCmd *imapclient.Command
				if barrier == "LOGIN" {
					barrierCmd = c.Login("alice", "secret")
				} else {
					barrierCmd = c.Logout()
				}

				switch kind {
				case "MultiSearch_independent":
					cmd := c.MultiSearch(
						&imap.SearchSource{Mailboxes: []string{"Archive"}},
						&imap.SearchCriteria{},
						&imap.SearchOptions{ReturnAll: true},
					)
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data) != 0 {
						t.Fatalf("pipelined MultiSearch stole pre-%s response: %+v", barrier, data[0])
					}
				case "UIDSearch_plain":
					cmd := c.UIDSearch(&imap.SearchCriteria{}, nil)
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if uids := data.AllUIDs(); len(uids) != 0 {
						t.Fatalf("pipelined UID SEARCH stole pre-%s response: %v", barrier, uids)
					}
				case "UIDSearch_ESEARCH":
					cmd := c.UIDSearch(&imap.SearchCriteria{}, &imap.SearchOptions{ReturnAll: true})
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if uids := data.AllUIDs(); len(uids) != 0 {
						t.Fatalf("pipelined UID SEARCH (ESEARCH) stole pre-%s response: %v", barrier, uids)
					}
				case "UIDSort_plain", "UIDSort_ESORT":
					cmd := c.UIDSort(&imapclient.SortOptions{
						SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDate}},
						SearchCriteria: &imap.SearchCriteria{},
					})
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data.UIDs) != 0 {
						t.Fatalf("pipelined %s stole pre-%s response: %v", kind, barrier, data.UIDs)
					}
				case "UIDSort_ESEARCH_reverse":
					cmd := c.UIDSort(&imapclient.SortOptions{
						SortCriteria:   []imap.SortCriterion{{Key: imap.SortKeyDate}},
						SearchCriteria: &imap.SearchCriteria{},
						Return:         imap.SortOptions{ReturnAll: true},
					})
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data.UIDs) != 0 {
						t.Fatalf("pipelined UID SORT (ESEARCH) stole pre-%s response: %v", barrier, data.UIDs)
					}
				case "UIDThread":
					cmd := c.UIDThread(&imapclient.ThreadOptions{
						Algorithm:      imap.ThreadReferences,
						SearchCriteria: &imap.SearchCriteria{},
					})
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if len(data) != 0 {
						t.Fatalf("pipelined UID THREAD stole pre-%s response: %+v", barrier, data)
					}
				case "UIDMove_COPYUID":
					cmd := c.Move(imap.UIDSetNum(1), "Archive")
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					data, err := cmd.Wait()
					if err != nil {
						t.Fatal(err)
					}
					if data.UIDValidity != 0 || len(data.UIDMapping) != 0 {
						t.Fatalf("pipelined UID MOVE stole pre-%s COPYUID: %+v", barrier, data)
					}
				case "UIDFetch":
					cmd := c.Fetch(imap.UIDSetNum(99), &imap.FetchOptions{UID: true, Flags: true})
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					msgs, err := cmd.Collect()
					if err != nil {
						t.Fatal(err)
					}
					if len(msgs) != 0 {
						t.Fatalf("pipelined UID FETCH stole pre-%s FETCH: %+v", barrier, msgs)
					}
				case "Expunge":
					cmd := c.Expunge()
					if err := barrierCmd.Wait(); err != nil {
						t.Fatal(err)
					}
					seqs, err := cmd.Collect()
					if err != nil {
						t.Fatal(err)
					}
					if len(seqs) != 0 {
						t.Fatalf("pipelined EXPUNGE stole pre-%s EXPUNGE: %v", barrier, seqs)
					}
				}

				c.Close()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// Across UNAUTHENTICATE + LOGIN (RFC 8437), responses emitted while User A's
// session is still active must not leak into User B's pipelined authenticated
// commands (LIST, LSUB, STATUS, METADATA, QUOTA, QUOTAROOT, ACL, NAMESPACE),
// while unsolicited notifications for User A before UNAUTHENTICATE completes
// must still reach UnilateralDataHandler.
func TestPipelinedUnauthenticateLoginBarrierBlocksCrossUserLeak(t *testing.T) {
	for _, kind := range []string{
		"List",
		"Lsub",
		"Status",
		"Metadata",
		"Quota",
		"QuotaRoot",
		"MyRights",
		"GetACL",
		"ListRights",
		"Namespace",
	} {
		t.Run(kind, func(t *testing.T) {
			const caps = "IMAP4rev2 UNAUTHENTICATE METADATA QUOTA ACL NAMESPACE"
			serve := func(br *bufio.Reader, conn net.Conn) error {
				unauthTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				loginTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				cmdTag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}

				// Emitted while User A is still authenticated (before UNAUTHENTICATE OK).
				var pre string
				switch kind {
				case "List":
					pre = "* LIST () \"/\" \"AliceSecret\"\r\n"
				case "Lsub":
					pre = "* LSUB () \"/\" \"AliceSecret\"\r\n"
				case "Status":
					pre = "* STATUS \"INBOX\" (MESSAGES 77 UIDNEXT 88)\r\n"
				case "Metadata":
					pre = "* METADATA \"INBOX\" (/private/comment \"alice-note\")\r\n* METADATA \"INBOX\" /private/comment\r\n"
				case "Quota":
					pre = "* QUOTA \"\" (STORAGE 50 100)\r\n"
				case "QuotaRoot":
					pre = "* QUOTAROOT \"INBOX\" \"\"\r\n* QUOTA \"\" (STORAGE 50 100)\r\n"
				case "MyRights":
					pre = "* MYRIGHTS \"INBOX\" lrswipkxtea\r\n"
				case "GetACL":
					pre = "* ACL \"INBOX\" alice lrswipkxtea\r\n"
				case "ListRights":
					pre = "* LISTRIGHTS \"INBOX\" bob l r s\r\n"
				case "Namespace":
					pre = "* NAMESPACE ((\"Alice/\" \"/\")) NIL NIL\r\n"
				}
				if _, err := fmt.Fprint(conn, pre); err != nil {
					return err
				}

				if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] unauthenticated\r\n", unauthTag, caps); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "%s OK [CAPABILITY %s] logged in as bob\r\n", loginTag, caps); err != nil {
					return err
				}
				if _, err := fmt.Fprintf(conn, "%s OK completed\r\n", cmdTag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			}

			log := &handlerLog{}
			c, done := newSelectIsolation307Peer(t, caps, &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					List:     func(*imap.ListData) { log.note("List") },
					Status:   func(*imap.StatusData) { log.note("Status") },
					Metadata: func(string, []string) { log.note("Metadata") },
				},
			}, serve)

			unauthCmd := c.Unauthenticate()
			loginCmd := c.Login("bob", "secret")

			switch kind {
			case "List":
				cmd := c.List("", "*", nil)
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				list, err := cmd.Collect()
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 0 {
					t.Fatalf("Bob's List received User A's pre-UNAUTHENTICATE mailbox: %+v", list[0])
				}
				if calls := log.take(); !slices.Equal(calls, []string{"List"}) {
					t.Fatalf("UnilateralDataHandler calls = %v, want [List]", calls)
				}
			case "Lsub":
				cmd := c.Lsub("", "*")
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				list, err := cmd.Collect()
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 0 {
					t.Fatalf("Bob's Lsub received User A's pre-UNAUTHENTICATE mailbox: %+v", list[0])
				}
			case "Status":
				cmd := c.Status("INBOX", &imap.StatusOptions{NumMessages: true})
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if data.NumMessages != nil || data.UIDNext != 0 {
					t.Fatalf("Bob's Status received User A's pre-UNAUTHENTICATE data: %+v", data)
				}
				if calls := log.take(); !slices.Equal(calls, []string{"Status"}) {
					t.Fatalf("UnilateralDataHandler calls = %v, want [Status]", calls)
				}
			case "Metadata":
				cmd := c.GetMetadata("INBOX", []string{"/private/comment"}, nil)
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if len(data.Entries) != 0 {
					t.Fatalf("Bob's GetMetadata received User A's pre-UNAUTHENTICATE entries: %+v", data.Entries)
				}
				if calls := log.take(); !slices.Equal(calls, []string{"Metadata"}) {
					t.Fatalf("UnilateralDataHandler calls = %v, want [Metadata]", calls)
				}
			case "Quota":
				cmd := c.GetQuota("")
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if data != nil {
					t.Fatalf("Bob's GetQuota received User A's pre-UNAUTHENTICATE quota: %+v", data)
				}
			case "QuotaRoot":
				cmd := c.GetQuotaRoot("INBOX")
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if len(data) != 0 {
					t.Fatalf("Bob's GetQuotaRoot received User A's pre-UNAUTHENTICATE quota: %+v", data)
				}
			case "MyRights":
				cmd := c.MyRights("INBOX")
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if data.Mailbox != "" || len(data.Rights) != 0 {
					t.Fatalf("Bob's MyRights received User A's pre-UNAUTHENTICATE rights: %+v", data)
				}
			case "GetACL":
				cmd := c.GetACL("INBOX")
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if data.Mailbox != "" || len(data.Rights) != 0 {
					t.Fatalf("Bob's GetACL received User A's pre-UNAUTHENTICATE ACL: %+v", data)
				}
			case "ListRights":
				cmd := c.ListRights("INBOX", "bob")
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if data.Mailbox != "" || len(data.RequiredRights) != 0 || len(data.OptionalRights) != 0 {
					t.Fatalf("Bob's ListRights received User A's pre-UNAUTHENTICATE rights: %+v", data)
				}
			case "Namespace":
				cmd := c.Namespace()
				if err := unauthCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				if err := loginCmd.Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := cmd.Wait()
				if err != nil {
					t.Fatal(err)
				}
				if len(data.Personal) != 0 {
					t.Fatalf("Bob's Namespace received User A's pre-UNAUTHENTICATE namespace: %+v", data)
				}
			}

			if err := c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
