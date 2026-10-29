package imapclient_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Empty sections remain readable during the callback, but their readers must
// enforce the same callback lifetime as non-empty sections. NIL has no reader.
func TestUnilateralFetchEmptyLiteralLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, attr, separator string
		nilLiteral            bool
	}{
		{"body literal", "BODY[] {0}\r\n", " ", false},
		{"body quoted", `BODY[] ""`, " ", false},
		{"binary literal", "BINARY[] ~{0}\r\n", " ", false},
		{"binary quoted", `BINARY[] ""`, " ", false},
		{"body literal missing space", "BODY[] {0}\r\n", "", false},
		{"binary literal missing space", "BINARY[] ~{0}\r\n", "", false},
		{"body NIL", "BODY[] NIL", " ", true},
		{"binary NIL", "BINARY[] NIL", " ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type result struct {
				literal imap.LiteralReader
				err     error
			}
			readers := make(chan result, 1)
			client := newUIDBufferLimitClient(t, &imapclient.Options{
				UnilateralDataHandler: &imapclient.UnilateralDataHandler{
					Fetch: func(msg *imapclient.FetchMessageData) {
						item := msg.Next()
						var literal imap.LiteralReader
						switch item := item.(type) {
						case imapclient.FetchItemDataBodySection:
							literal = item.Literal
						case imapclient.FetchItemDataBinarySection:
							literal = item.Literal
						default:
							readers <- result{err: fmt.Errorf("first item = %T, want body or binary section", item)}
							return
						}
						if (literal == nil) != tc.nilLiteral {
							readers <- result{err: fmt.Errorf("literal is nil = %v, want %v", literal == nil, tc.nilLiteral)}
							return
						}
						if literal != nil {
							if literal.Size() != 0 {
								readers <- result{err: fmt.Errorf("literal size = %d, want 0", literal.Size())}
								return
							}
							b, err := io.ReadAll(literal)
							if err != nil || len(b) != 0 {
								readers <- result{err: fmt.Errorf("read during callback = %q, %v; want empty body", b, err)}
								return
							}
						}
						readers <- result{literal: literal}
						// Leave UID and the following non-empty section unread.
					},
				},
			}, func(conn net.Conn, reader *bufio.Reader) error {
				line, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				tag, command, _ := strings.Cut(line, " ")
				if command != "NOOP\r\n" {
					return fmt.Errorf("expected NOOP, got %q", line)
				}
				_, err = fmt.Fprintf(conn, "* 2 FETCH (%s%sUID 6 BODY[99] {1}\r\nx)\r\n%s OK noop\r\n", tc.attr, tc.separator, tag)
				return err
			})
			// The final non-empty body blocks decoding until callback cleanup
			// drains it, so completion proves the callback lifetime has ended.
			if err := client.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
			got := <-readers
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.literal != nil {
				if _, err := io.ReadAll(got.literal); err == nil || !strings.Contains(err.Error(), "callback") {
					t.Errorf("read after callback = %v, want callback lifetime error", err)
				}
			}
		})
	}
}
