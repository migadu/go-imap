package imapserver

import (
	"bytes"
	"testing"

	"github.com/migadu/go-imap"
)

// TestWriteThreadWireFormat pins the THREAD response to the RFC 5256 section 5
// grammar:
//
//	thread-data    = "THREAD" [SP 1*thread-list]
//	thread-list    = "(" (thread-members / thread-nested) ")"
//	thread-members = nz-number *(SP nz-number) [SP thread-nested]
//	thread-nested  = 2*thread-list
//
// Thread-lists follow one another with nothing between them, at the top level
// as well as inside thread-nested. The writer used to put an SP between the
// top-level ones, and Roundcube, which reads the response by matching
// parentheses, then kept the first thread and dropped all the others: its
// threaded message list showed one conversation per folder.
func TestWriteThreadWireFormat(t *testing.T) {
	cases := []struct {
		name string
		data []imap.ThreadData
		want string
	}{
		{
			name: "no threads",
			data: nil,
			want: "* THREAD\r\n",
		},
		{
			name: "single message",
			data: []imap.ThreadData{{Chain: []uint32{1}}},
			want: "* THREAD (1)\r\n",
		},
		{
			name: "several threads",
			data: []imap.ThreadData{
				{Chain: []uint32{1}},
				{Chain: []uint32{2, 3}},
				{Chain: []uint32{4}},
			},
			want: "* THREAD (1)(2 3)(4)\r\n",
		},
		{
			// The REFERENCES example in RFC 5256 section 4.
			name: "RFC 5256 example",
			data: []imap.ThreadData{
				{Chain: []uint32{2}},
				{Chain: []uint32{3, 6}, SubThreads: []imap.ThreadData{
					{Chain: []uint32{4, 23}},
					{Chain: []uint32{44, 7, 96}},
				}},
			},
			want: "* THREAD (2)(3 6 (4 23)(44 7 96))\r\n",
		},
		{
			// A thread whose root is missing: thread-nested on its own.
			name: "missing root",
			data: []imap.ThreadData{
				{SubThreads: []imap.ThreadData{
					{Chain: []uint32{3}},
					{Chain: []uint32{5}},
				}},
				{Chain: []uint32{7}},
			},
			want: "* THREAD ((3)(5))(7)\r\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
		})
	}
}
