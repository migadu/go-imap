package imapclient_test

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/migadu/go-imap"
)

func TestMoveFallbackCopyUIDRoutingIsRaceFree(t *testing.T) {
	const rounds = 100
	c, done := newSelectIsolation307Peer(t, "IMAP4rev1 UIDPLUS", nil, func(br *bufio.Reader, conn net.Conn) error {
		tag, err := readSelectIsolation307Command(br)
		if err != nil {
			return err
		}
		if err = writeSelectIsolation307Data(conn, tag, 3, `\Seen`, `\Seen`); err != nil {
			return err
		}
		for range rounds {
			copyTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			// An unsolicited native-MOVE response must not race with a fallback
			// command still publishing its STORE/EXPUNGE companions.
			if _, err = fmt.Fprint(conn, "* OK [COPYUID 7 1 31] unsolicited move result\r\n"); err != nil {
				return err
			}
			storeTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			expungeTag, err := readSelectIsolation307Command(br)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(conn, "%s OK [COPYUID 7 1 32] copy\r\n%s OK store\r\n%s OK expunge\r\n", copyTag, storeTag, expungeTag); err != nil {
				return err
			}
		}
		return finishSelectIsolation307Noop(br, conn)
	})
	if _, err := c.Select("Old", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	for range rounds {
		data, err := c.Move(imap.SeqSetNum(1), "Archive").Wait()
		if err != nil {
			t.Fatal(err)
		}
		want := imap.UIDMapping{{Source: imap.UIDRange{Start: 1, Stop: 1}, Dest: imap.UIDRange{Start: 32, Stop: 32}}}
		if data.UIDValidity != 7 || !reflect.DeepEqual(data.UIDMapping, want) {
			t.Fatalf("tagged COPYUID lost: %+v", data)
		}
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNativeMoveUntaggedCopyUIDRouting(t *testing.T) {
	for _, numSet := range []imap.NumSet{imap.SeqSetNum(1), imap.UIDSetNum(1)} {
		t.Run(fmt.Sprintf("%T", numSet), func(t *testing.T) {
			c, done := newSelectIsolation307Peer(t, "IMAP4rev1 MOVE UIDPLUS", nil, func(br *bufio.Reader, conn net.Conn) error {
				tag, err := readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if err = writeSelectIsolation307Data(conn, tag, 3, `\Seen`, `\Seen`); err != nil {
					return err
				}
				tag, err = readSelectIsolation307Command(br)
				if err != nil {
					return err
				}
				if _, err = fmt.Fprintf(conn, "* OK [COPYUID 7 1 31] moved\r\n%s OK move\r\n", tag); err != nil {
					return err
				}
				return finishSelectIsolation307Noop(br, conn)
			})
			if _, err := c.Select("Old", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := c.Move(numSet, "Archive").Wait()
			if err != nil {
				t.Fatal(err)
			}
			want := imap.UIDMapping{{Source: imap.UIDRange{Start: 1, Stop: 1}, Dest: imap.UIDRange{Start: 31, Stop: 31}}}
			if data.UIDValidity != 7 || !reflect.DeepEqual(data.UIDMapping, want) {
				t.Fatalf("native MOVE COPYUID lost: %+v", data)
			}
			if err = c.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
