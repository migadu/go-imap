package imapclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func TestReadCapabilitiesWhitespace(t *testing.T) {
	for _, ending := range []string{"\r\n", "\n", "] response text\r\n"} {
		for _, trailing := range []int{0, 1, 2, 8} {
			t.Run(fmt.Sprintf("ending=%q/spaces=%d", ending, trailing), func(t *testing.T) {
				br := bufio.NewReader(strings.NewReader(" IMAP4rev1  IDLE RIGHTS=kxte X-example" + strings.Repeat(" ", trailing) + ending + "next\r\n"))
				dec := imapwire.NewDecoder(br, imapwire.ConnSideClient)
				caps, err := readCapabilities(dec)
				if err != nil {
					t.Fatal(err)
				}
				for _, cap := range []imap.Cap{imap.CapIMAP4rev1, imap.CapIdle, imap.Cap("RIGHTS=kxte"), imap.Cap("X-EXAMPLE")} {
					if _, ok := caps[cap]; !ok {
						t.Errorf("missing %s from %v", cap, caps)
					}
				}
				if len(caps) != 4 {
					t.Errorf("caps = %v, want exactly four capabilities", caps)
				}
				rest, err := io.ReadAll(br)
				if err != nil || string(rest) != ending+"next\r\n" {
					t.Errorf("remaining input = %q, %v; want %q", rest, err, ending+"next\r\n")
				}
			})
		}
	}
}

func TestReadCapabilitiesReadErrors(t *testing.T) {
	wantIO := errors.New("CAPABILITY source failure")
	for _, tc := range []struct {
		name   string
		source io.Reader
		limit  int64
		want   error
	}{
		{name: "EOF after capability", source: strings.NewReader(" IMAP4rev1 IDLE"), want: io.ErrUnexpectedEOF},
		{name: "EOF after separator", source: strings.NewReader(" IMAP4rev1 "), want: io.ErrUnexpectedEOF},
		{name: "EOF after extra separators", source: strings.NewReader(" IMAP4rev1    "), want: io.ErrUnexpectedEOF},
		{name: "error after capability", source: io.MultiReader(strings.NewReader(" IMAP4rev1"), iotest.ErrReader(wantIO)), want: wantIO},
		{name: "error after separator", source: io.MultiReader(strings.NewReader(" IMAP4rev1 "), iotest.ErrReader(wantIO)), want: wantIO},
		{name: "whitespace cannot bypass response limit", source: strings.NewReader(" IMAP4rev1" + strings.Repeat(" ", 128) + "\r\n"), limit: 32, want: imapwire.ErrMaxSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(tc.source), imapwire.ConnSideClient)
			dec.MaxSize = tc.limit
			if _, err := readCapabilities(dec); !errors.Is(err, tc.want) {
				t.Fatalf("readCapabilities() error = %v, want %v", err, tc.want)
			}
			if !errors.Is(dec.Err(), tc.want) {
				t.Errorf("decoder error = %v, want %v", dec.Err(), tc.want)
			}
		})
	}
	// A failed decoder may not become a successful empty capability response.
	br := bufio.NewReader(strings.NewReader(" IMAP4rev1\r\n"))
	dec := imapwire.NewDecoder(br, imapwire.ConnSideClient)
	dec.Expect(false, "injected failure")
	want := dec.Err()
	if caps, err := readCapabilities(dec); err != want || len(caps) != 0 {
		t.Fatalf("readCapabilities() after failure = %v, %v; want empty, %v", caps, err, want)
	}
	if rest, _ := io.ReadAll(br); string(rest) != " IMAP4rev1\r\n" {
		t.Errorf("failed decoder consumed input: %q", rest)
	}
}

func TestReadCapabilitiesMalformedValue(t *testing.T) {
	// The old SP workaround loops without consuming an opening parenthesis,
	// even after its source is closed. Isolate this check so a regression is
	// bounded and cannot leave a CPU-spinning goroutine in the test process.
	const childEnv = "GOIMAP_CAPABILITY_TEST_SUBPROCESS"
	if os.Getenv(childEnv) != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestReadCapabilitiesMalformedValue$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1", "GORACE=atexit_sleep_ms=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("malformed CAPABILITY parser failed or did not finish: %v, %v\n%s", err, ctx.Err(), output)
		}
		return
	}
	for _, value := range []string{"(IDLE)", "\"IDLE\"", "*", "%", "\tIDLE", "{4}\r\nIDLE"} {
		t.Run(value, func(t *testing.T) {
			dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(" IMAP4rev1 "+value+"\r\n")), imapwire.ConnSideClient)
			_, err := readCapabilities(dec)
			var syntaxErr *imapwire.DecoderExpectError
			if !errors.As(err, &syntaxErr) {
				t.Fatalf("readCapabilities() error = %v, want a syntax error", err)
			}
		})
	}
}
