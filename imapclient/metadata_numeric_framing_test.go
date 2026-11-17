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

func TestMetadataNumericResponseCodeFraming(t *testing.T) {
	tests := []struct {
		code     string
		status   string
		wantSize *uint32
	}{
		{code: "METADATA LONGENTRIES 256", wantSize: metadataSize(256)},
		{code: "metadata longentries 256", wantSize: metadataSize(256)},
		{code: "MeTaDaTa LoNgEnTrIeS 0", wantSize: metadataSize(0)},
		{code: "METADATA MAXSIZE 123junk", status: "NO"},
		{code: "metadata maxsize 256 junk", status: "NO"},
	}
	for _, prefix := range []string{
		"METADATA LONGENTRIES", "metadata longentries",
		"METADATA MAXSIZE", "metadata maxsize",
	} {
		for _, suffix := range []string{" 256 junk", " 123junk", " 4294967296", " nope", ""} {
			tests = append(tests, struct {
				code     string
				status   string
				wantSize *uint32
			}{code: prefix + suffix})
		}
	}

	for _, tt := range tests {
		t.Run(tt.status+" "+tt.code, func(t *testing.T) {
			status := tt.status
			if status == "" {
				status = "OK"
			}
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))

			go func() {
				br := bufio.NewReader(serverConn)
				bw := bufio.NewWriter(serverConn)
				_, _ = bw.WriteString("* PREAUTH [CAPABILITY IMAP4rev2 METADATA] ready\r\n")
				_ = bw.Flush()
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ := strings.Cut(line, " ")
				_, _ = fmt.Fprintf(bw, "* METADATA INBOX (/private/comment \"actual\")\r\n%s %s [%s] metadata done\r\n", tag, status, tt.code)
				_ = bw.Flush()

				line, err = br.ReadString('\n')
				if err != nil {
					return
				}
				tag, _, _ = strings.Cut(line, " ")
				_, _ = fmt.Fprintf(bw, "%s OK still framed\r\n", tag)
				_ = bw.Flush()
			}()

			c := imapclient.New(clientConn, &imapclient.Options{ResponseTimeout: 4 * time.Second})
			defer c.Close()
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			data, err := c.GetMetadata("INBOX", []string{"/private/comment"}, nil).Wait()
			if status == "NO" {
				var imapErr *imap.Error
				if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseTypeNo || imapErr.Text != "metadata done" {
					t.Fatalf("GetMetadata error = %v, want tagged NO with original text", err)
				}
			} else if err != nil {
				t.Fatalf("GetMetadata must ignore an unusable advisory code: %v", err)
			}
			if value := data.Entries["/private/comment"]; value == nil || string(*value) != "actual" {
				t.Fatalf("metadata value = %v, want actual", value)
			}
			if tt.wantSize == nil {
				if data.ResponseCodeData != nil {
					t.Errorf("malformed numeric code published partial data: %+v", data.ResponseCodeData)
				}
			} else if data.ResponseCodeData == nil || data.ResponseCodeData.Size != *tt.wantSize {
				t.Errorf("ResponseCodeData = %+v, want Size=%d", data.ResponseCodeData, *tt.wantSize)
			}
			if err := c.Noop().Wait(); err != nil {
				t.Fatalf("NOOP after METADATA: %v", err)
			}
		})
	}
}

func metadataSize(size uint32) *uint32 {
	return &size
}
