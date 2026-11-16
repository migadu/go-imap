package imapserver

import (
	"bufio"
	"bytes"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
)

func TestFetchFlagsRecentRevisionAtWrite(t *testing.T) {
	for _, mode := range []string{"rev1", "rev1_only", "utf8_only", "enabled_rev2", "rev2_only"} {
		for _, flags := range [][]imap.Flag{{"\\Recent", imap.FlagSeen, "keyword", "\\rEcEnT"}, {"\\Recent"}, {"Recent", "$Recent", "\\Recentish", "\\Recent"}, {}} {
			t.Run(mode+"/"+strings.Join(recentWriterFlagStrings(flags), ","), func(t *testing.T) {
				local, peer := net.Pipe()
				t.Cleanup(func() { local.Close(); peer.Close() })
				var out bytes.Buffer
				caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}
				if mode == "rev1_only" {
					delete(caps, imap.CapIMAP4rev2)
				}
				if mode == "rev2_only" {
					delete(caps, imap.CapIMAP4rev1)
				}
				conn := &Conn{conn: local, bw: bufio.NewWriter(&out), server: New(&Options{Caps: caps})}
				// The backend's collected flags outlive a revision transition.
				before := append([]imap.Flag{}, flags...)
				if mode == "enabled_rev2" {
					conn.enabled = imap.CapSet{imap.CapIMAP4rev2: {}}
				} else if mode == "utf8_only" {
					conn.enabled = imap.CapSet{imap.CapUTF8Accept: {}}
				}
				w := (&FetchWriter{conn: conn}).CreateMessage(1)
				w.WriteFlags(flags)
				w.WriteUID(7)
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				want := "* 1 FETCH (FLAGS ("
				var kept []string
				for _, flag := range before {
					if (mode == "enabled_rev2" || mode == "rev2_only") && strings.EqualFold(string(flag), "\\Recent") {
						continue
					}
					kept = append(kept, string(flag))
				}
				want += strings.Join(kept, " ") + ") UID 7)\r\n"
				if got := out.String(); got != want {
					t.Fatalf("FLAGS: got %q, want %q", got, want)
				}
				if !reflect.DeepEqual(append([]imap.Flag{}, flags...), before) {
					t.Fatalf("writer changed caller flags: %v -> %v", before, flags)
				}
			})
		}
	}
}

func recentWriterFlagStrings(flags []imap.Flag) []string {
	values := make([]string, len(flags))
	for i, flag := range flags {
		values[i] = string(flag)
	}
	return values
}
