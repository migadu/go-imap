package imapserver_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

type binaryFetchMemoryRecorder struct {
	armed   atomic.Bool
	blocked chan struct{}
	resume  chan struct{}
}

func (*binaryFetchMemoryRecorder) ClientBytes([]byte) {}

func (r *binaryFetchMemoryRecorder) ServerBytes(p []byte) {
	if bytes.Contains(p, []byte(" FETCH (")) && r.armed.CompareAndSwap(true, false) {
		close(r.blocked)
		<-r.resume
	}
}

func TestFetchBinaryPartialsBoundRetainedMemory(t *testing.T) {
	// A small result must not keep a complete decoded copy of the same part
	// alive for every requested partial. Pause the actual FETCH output while
	// its preceding header is written, before the binary items can be sent.
	// This measures live memory through the public server path rather than
	// prescribing how the backend validates or buffers binary sections.
	recorder := &binaryFetchMemoryRecorder{blocked: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(recorder.resume) })
	defer resume()

	user := imapmemserver.NewUser("user", "pass")
	if err := user.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}},
		InsecureAuth: true,
		NewDebugConn: func(net.Conn) imapserver.DebugConn { return recorder },
	})
	t.Cleanup(func() { srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	client := dialNotifyTest(t, ln.Addr().String())
	defer client.close()
	rc := &searchRev2Conn{t: t, c: client.conn, br: client.br}
	rc.do("LOGIN user pass")

	const partSize = 1 << 20
	const partials = 64
	// The header is larger than the response buffer, so the recorder pauses
	// before any binary item is written, even though their combined payload
	// is only 64 bytes. Use distinct offsets, not duplicate fetch attributes.
	header := strings.Repeat("X-Padding: "+strings.Repeat("p", 90)+"\r\n", 128)
	body := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'a'}, partSize))
	rc.appendMessage(header + singlePart("application/octet-stream", "base64", body))
	rc.do("SELECT INBOX")
	var command strings.Builder
	command.WriteString("t99 FETCH 1 (BODY.PEEK[HEADER]")
	for i := range partials {
		fmt.Fprintf(&command, " BINARY.PEEK[1]<%d.1>", i)
	}
	command.WriteString(")\r\n")

	runtime.GC()
	var before, during runtime.MemStats
	runtime.ReadMemStats(&before)
	recorder.armed.Store(true)
	if _, err := rc.c.Write([]byte(command.String())); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorder.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("FETCH did not reach its first response write")
	}
	runtime.GC()
	runtime.ReadMemStats(&during)
	resume()

	var response strings.Builder
	for {
		line, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		response.WriteString(line)
		if strings.HasPrefix(line, "t99 ") {
			if !strings.Contains(line, " OK ") {
				t.Fatalf("FETCH completion: %q", line)
			}
			break
		}
	}
	for i := range partials {
		if !strings.Contains(response.String(), fmt.Sprintf("BINARY[1]<%d> {1}\r\na", i)) {
			t.Fatalf("FETCH omitted or corrupted binary partial %d", i)
		}
	}
	rc.do("NOOP")

	retained := int64(during.HeapAlloc) - int64(before.HeapAlloc)
	// Allow 32 times the decoded part size for protocol buffers, GC noise,
	// and alternative bounded implementations. Retaining a separate backing
	// buffer per one-byte partial needs at least 64 MiB (128 MiB today).
	if retained > 32*partSize {
		t.Fatalf("FETCH retained %.1f MiB for %d one-byte partials of a 1 MiB part; limit is 32 MiB", float64(retained)/(1<<20), partials)
	}
	t.Logf("FETCH retained %.1f MiB for %d one-byte partials", float64(retained)/(1<<20), partials)
}
