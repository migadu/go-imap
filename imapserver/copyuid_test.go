package imapserver

import (
	"bytes"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestCopyUIDOmitsInvalidMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		data imap.CopyData
	}{
		{"nil mapping", imap.CopyData{UIDValidity: 1}},
		{"empty mapping", imap.CopyData{UIDValidity: 1, UIDMapping: imap.UIDMapping{}}},
		{"unequal lengths", imap.CopyData{UIDValidity: 1, UIDMapping: imap.UIDMapping{{Source: imap.UIDRange{Start: 1, Stop: 2}, Dest: imap.UIDRange{Start: 3, Stop: 3}}}}},
		{"zero UID", imap.CopyData{UIDValidity: 1, UIDMapping: imap.UIDMapping{{Source: imap.UIDRange{}, Dest: imap.UIDRange{Start: 3, Stop: 3}}}}},
		{"zero epoch", imap.CopyData{UIDMapping: imap.UIDMapping{{Source: imap.UIDRange{Start: 1, Stop: 1}, Dest: imap.UIDRange{Start: 3, Stop: 3}}}}},
		{"duplicate", imap.CopyData{UIDValidity: 1, UIDMapping: imap.UIDMapping{{Source: imap.UIDRange{Start: 1, Stop: 2}, Dest: imap.UIDRange{Start: 3, Stop: 4}}, {Source: imap.UIDRange{Start: 2, Stop: 2}, Dest: imap.UIDRange{Start: 5, Stop: 5}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, move := range []bool{false, true} {
				var out bytes.Buffer
				conn := newACLTestConn(t, nil, &out)
				tag := "T1"
				var err error
				if move {
					tag = "*"
					err = (&MoveWriter{conn: conn}).WriteCopyData(&tc.data)
				} else {
					err = conn.writeCopyOK(tag, &tc.data)
				}
				if err != nil {
					t.Fatal(err)
				}
				if out.String() != tag+" OK COPY completed\r\n" {
					t.Fatalf("invalid advisory mapping affected successful COPY/MOVE: %q", out.String())
				}
			}
		})
	}
}

func TestCopyUIDOrderedMappingEncoding(t *testing.T) {
	for _, move := range []bool{false, true} {
		var out bytes.Buffer
		conn := newACLTestConn(t, nil, &out)
		data := &imap.CopyData{UIDValidity: 7}
		for _, pair := range [][2]imap.UID{{4, 101}, {1, 102}, {2, 110}, {3, 111}} {
			data.UIDMapping.Add(pair[0], pair[1])
		}
		tag := "T1"
		var err error
		if move {
			tag = "*"
			err = (&MoveWriter{conn: conn}).WriteCopyData(data)
		} else {
			err = conn.writeCopyOK(tag, data)
		}
		if err != nil {
			t.Fatal(err)
		}
		want := tag + " OK [COPYUID 7 4,1:3 101:102,110:111] COPY completed\r\n"
		if out.String() != want {
			t.Fatalf("wire=%q; want=%q", out.String(), want)
		}
	}
}
