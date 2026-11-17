package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
)

func TestStoreModifiedPreservesSearchResultMarker(t *testing.T) {
	for _, uid := range []bool{false, true} {
		for _, status := range []string{"OK", "NO"} {
			for _, marker := range []string{"$", "$junk", "$ junk"} {
				t.Run(fmt.Sprintf("uid=%v/%s/%s", uid, status, marker), func(t *testing.T) {
					clientConn, serverConn := net.Pipe()
					defer clientConn.Close()
					defer serverConn.Close()
					_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
					_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

					go func() {
						br := bufio.NewReader(serverConn)
						_, _ = fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev2 CONDSTORE SEARCHRES ESEARCH] ready\r\n")
						for stage := 0; stage < 4; stage++ {
							line, err := br.ReadString('\n')
							if err != nil {
								return
							}
							tag, command, _ := strings.Cut(line, " ")
							switch stage {
							case 0:
								_, _ = fmt.Fprintf(serverConn, "* 2 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-WRITE] selected\r\n", tag)
							case 1:
								want := "SEARCH RETURN (SAVE) ALL\r\n"
								if uid {
									want = "UID " + want
								}
								if command != want {
									_, _ = fmt.Fprintf(serverConn, "%s BAD expected %q, got %q\r\n", tag, want, command)
									return
								}
								// Both messages match ALL; SAVE alone suppresses results.
								_, _ = fmt.Fprintf(serverConn, "%s OK saved messages 1 and 2\r\n", tag)
							case 2:
								_, _ = fmt.Fprintf(serverConn, "%s %s [MODIFIED %s] conditional result\r\n", tag, status, marker)
							case 3:
								_, _ = fmt.Fprintf(serverConn, "%s OK still framed\r\n", tag)
							}
						}
					}()

					c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
					defer c.Close()
					if err := c.WaitGreeting(); err != nil {
						t.Fatal(err)
					}
					if _, err := c.Select("INBOX", nil).Wait(); err != nil {
						t.Fatal(err)
					}
					if err := c.SaveSearchForTest(uid); err != nil {
						t.Fatalf("SEARCH SAVE setup: %v", err)
					}
					var nums imap.NumSet = imap.SeqSetNum(1, 2)
					if uid {
						nums = imap.UIDSetNum(1, 2)
					}
					cmd := c.Store(nums, &imap.StoreFlags{
						Op:     imap.StoreFlagsAdd,
						Silent: true,
						Flags:  []imap.Flag{imap.FlagSeen},
					}, &imap.StoreOptions{UnchangedSince: 10})
					_, err := cmd.Collect()
					if status == "NO" {
						var imapErr *imap.Error
						if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo || imapErr.Text != "conditional result" {
							t.Fatalf("Store error = %v, want original tagged NO", err)
						}
						wantCode := imap.ResponseCode("")
						if marker == "$" {
							wantCode = imap.ResponseCodeModified
						}
						if imapErr.Code != wantCode {
							t.Errorf("response code = %q, want %q", imapErr.Code, wantCode)
						}
					} else if err != nil {
						t.Fatalf("Store error = %v, want tagged OK", err)
					}

					modified := cmd.Modified()
					if marker == "$" {
						// Check nil first: IsSearchRes requires a number set.
						if modified == nil {
							t.Error("MODIFIED $ was discarded")
						} else if !imap.IsSearchRes(modified) || modified.String() != "$" {
							t.Errorf("Modified() = %T(%v), want SEARCHRES marker", modified, modified)
						}
					} else if modified != nil {
						t.Errorf("malformed %q became Modified() = %v", marker, modified)
					}
					if err := c.Noop().Wait(); err != nil {
						t.Fatalf("NOOP after MODIFIED: %v", err)
					}
				})
			}
		}
	}
}
