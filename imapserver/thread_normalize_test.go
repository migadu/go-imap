package imapserver

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/migadu/go-imap"
)

// TestWriteThreadNormalizesLists checks that a tree the RFC 5256 section 5
// grammar cannot express directly is written in a form it can. thread-nested
// is 2*thread-list and thread-members starts with an nz-number, so a node
// with a single sub-thread is written as one chain, (1 (2)) as (1 2), and a
// node without messages or sub-threads is left out. Dovecot's writer does the
// same, as "a special case to avoid extra parenthesis".
func TestWriteThreadNormalizesLists(t *testing.T) {
	one := func(n uint32) imap.ThreadData { return imap.ThreadData{Chain: []uint32{n}} }

	cases := []struct {
		name string
		data []imap.ThreadData
		want string
	}{
		{
			name: "empty node",
			data: []imap.ThreadData{{}},
			want: "* THREAD\r\n",
		},
		{
			name: "empty node among others",
			data: []imap.ThreadData{one(1), {}, one(2)},
			want: "* THREAD (1)(2)\r\n",
		},
		{
			name: "chain with one sub-thread",
			data: []imap.ThreadData{{Chain: []uint32{1, 2}, SubThreads: []imap.ThreadData{one(3)}}},
			want: "* THREAD (1 2 3)\r\n",
		},
		{
			name: "one sub-thread without chain",
			data: []imap.ThreadData{{SubThreads: []imap.ThreadData{one(3)}}},
			want: "* THREAD (3)\r\n",
		},
		{
			name: "one nested sub-thread",
			data: []imap.ThreadData{{SubThreads: []imap.ThreadData{
				{SubThreads: []imap.ThreadData{one(3), one(4)}},
			}}},
			want: "* THREAD ((3)(4))\r\n",
		},
		{
			name: "one sub-thread with sub-threads of its own",
			data: []imap.ThreadData{{Chain: []uint32{1}, SubThreads: []imap.ThreadData{
				{Chain: []uint32{2}, SubThreads: []imap.ThreadData{one(3), one(4)}},
			}}},
			want: "* THREAD (1 2 (3)(4))\r\n",
		},
		{
			name: "single sub-thread deep down",
			data: []imap.ThreadData{{Chain: []uint32{1}, SubThreads: []imap.ThreadData{
				one(2),
				{Chain: []uint32{3}, SubThreads: []imap.ThreadData{one(4)}},
			}}},
			want: "* THREAD (1 (2)(3 4))\r\n",
		},
		{
			name: "empty sub-thread leaves a single one",
			data: []imap.ThreadData{{Chain: []uint32{1}, SubThreads: []imap.ThreadData{{}, one(2)}}},
			want: "* THREAD (1 2)\r\n",
		},
		{
			name: "already in form",
			data: []imap.ThreadData{
				one(2),
				{Chain: []uint32{3, 6}, SubThreads: []imap.ThreadData{
					{Chain: []uint32{4, 23}},
					{Chain: []uint32{44, 7, 96}},
				}},
			},
			want: "* THREAD (2)(3 6 (4 23)(44 7 96))\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fmt.Sprintf("%+v", tc.data)

			var out bytes.Buffer
			c := newACLTestConn(t, nil, &out)
			if err := c.writeThread(tc.data); err != nil {
				t.Fatalf("writeThread() = %v", err)
			}
			if err := c.bw.Flush(); err != nil {
				t.Fatalf("Flush() = %v", err)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("writeThread() wrote %q, want %q", got, tc.want)
			}
			if after := fmt.Sprintf("%+v", tc.data); after != before {
				t.Errorf("writeThread() changed its argument to %s", after)
			}
		})
	}
}
