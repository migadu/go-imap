package imapclient

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// TestReadBodyStructureDepthLimit is a regression test for unbounded recursion
// while parsing BODYSTRUCTURE.
//
// A body structure nests: a multipart holds bodies, and a message/rfc822 part
// holds another body. readBody recursed through both without a depth limit. The
// decoder's own maxListDepth does not apply, because a body is opened with
// ExpectSpecial('(') rather than through Decoder.List, so nothing counted the
// nesting.
//
// A server is not required to send anything sane here, and this is remotely
// reachable on any FETCH that asks for BODYSTRUCTURE.
//
// The concrete harm is the error path: every level wraps the error from the
// level below with fmt.Errorf, so a nesting that fails anywhere inside costs
// time and memory quadratic in the depth. Measured before the fix, on nesting
// that ends in a parse error:
//
//	  5000 levels   0.04s
//	 10000 levels   0.13s
//	 20000 levels   0.51s
//	 40000 levels   2.14s
//	100000 levels   did not finish within 60s
//
// maxResponseSize caps a response at 8 MiB, which is hundreds of thousands of
// levels, so a single response is enough to tie the connection up indefinitely.
//
// The recursion depth itself was also unbounded. That one did not crash in
// testing -- Go grew the stack to 800000 frames without overflowing -- but
// nothing was keeping it in check either.
//
// Upstream: emersion/go-imap#574 asks for exactly this ("Recursion limits for
// BODYSTRUCTURE and SEARCH"). SEARCH is already bounded on the server side by
// maxSearchKeyDepth.
func TestReadBodyStructureDepthLimit(t *testing.T) {
	// Deep enough that the old quadratic path takes seconds, so a regression
	// shows up as a timeout rather than passing slowly.
	const depth = 100000
	// Valid nesting: every level is (<child> "MIXED"), so the limit is proven
	// to trigger on well-formed input rather than on a parse error.
	data := strings.Repeat("(", depth) +
		`("TEXT" "PLAIN" NIL NIL NIL "7BIT" 10 1)` +
		strings.Repeat(` "MIXED")`, depth)

	done := make(chan error, 1)
	go func() {
		dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
		_, err := readBody(dec, &Options{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readBody() = nil, want a depth-limit error")
		}
		if !strings.Contains(err.Error(), "nesting too deep") {
			t.Fatalf("readBody() = %v, want a depth-limit error", err)
		}
		// The error must not carry a wrapper per level.
		if n := strings.Count(err.Error(), "body-type-mpart"); n > 2*maxBodyStructureDepth {
			t.Errorf("error wraps %v levels, want at most %v", n, 2*maxBodyStructureDepth)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("readBody() did not finish in 20s: nesting is not bounded")
	}
}

// TestReadBodyStructureNestingAllowed is the guard: nesting that any real
// message could plausibly use must still parse.
func TestReadBodyStructureNestingAllowed(t *testing.T) {
	// 20 levels of multipart, far beyond anything real mail produces.
	const depth = 20
	data := strings.Repeat("(", depth) +
		`("TEXT" "PLAIN" NIL NIL NIL "7BIT" 10 1)` +
		strings.Repeat(` "MIXED")`, depth)

	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(data)), imapwire.ConnSideClient)
	if _, err := readBody(dec, &Options{}); err != nil {
		t.Fatalf("readBody() at depth %v = %v, want success", depth, err)
	}
}

// TestReadBodyStructureMessageNestingCounted checks the other recursion path:
// message/rfc822 parts nest through readBodyType1part, not readBodyTypeMpart.
func TestReadBodyStructureMessageNestingCounted(t *testing.T) {
	const depth = 100000
	var sb strings.Builder
	for i := 0; i < depth; i++ {
		sb.WriteString(`("MESSAGE" "RFC822" NIL NIL NIL "7BIT" 10 (NIL "s" NIL NIL NIL NIL NIL NIL NIL NIL) `)
	}
	sb.WriteString(`("TEXT" "PLAIN" NIL NIL NIL "7BIT" 10 1)`)
	for i := 0; i < depth; i++ {
		sb.WriteString(` 1)`)
	}

	done := make(chan error, 1)
	go func() {
		dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(sb.String())), imapwire.ConnSideClient)
		_, err := readBody(dec, &Options{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "nesting too deep") {
			t.Fatalf("readBody() = %v, want a depth-limit error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("readBody() did not finish in 20s: message nesting is not bounded")
	}
}
