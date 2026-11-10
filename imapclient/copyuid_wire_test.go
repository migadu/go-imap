package imapclient_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestCopyUIDWireOrder(t *testing.T) {
	for _, command := range []string{"COPY", "MOVE", "MOVE fallback"} {
		for _, code := range []string{
			" 7 4,1:3 101:102,110:111", " 7 1:* 101:102", " 7 1:2 101",
			"", " ", " 7", " 7 1", " 7 1 ", " nope 1 101",
			" 4294967296 1 101", " 7 1 101 extra", " 7 (1) 101", " 7 1 \"101\"",
		} {
			for _, status := range []string{"OK", "NO", "BAD"} {
				t.Run(command+"/"+status+"/"+code, func(t *testing.T) {
					clientConn, serverConn := net.Pipe()
					clientConn.SetDeadline(time.Now().Add(5 * time.Second))
					serverConn.SetDeadline(time.Now().Add(5 * time.Second))
					t.Cleanup(func() { clientConn.Close(); serverConn.Close() })
					go func() {
						defer serverConn.Close()
						caps := "IMAP4rev1 UIDPLUS"
						if command == "MOVE" {
							caps += " MOVE"
						}
						fmt.Fprintf(serverConn, "* PREAUTH [CAPABILITY %s] ready\r\n", caps)
						reader := bufio.NewReader(serverConn)
						for {
							line, err := reader.ReadString('\n')
							if err != nil {
								return
							}
							fields := strings.Fields(line)
							tag := fields[0]
							if strings.Contains(line, " MOVE ") {
								fmt.Fprintf(serverConn, "* OK [COPYUID%s] moved\r\n", code)
								if status == "OK" {
									io.WriteString(serverConn, "* 1 EXPUNGE\r\n")
								}
								fmt.Fprintf(serverConn, "%s %s completed\r\n", tag, status)
								continue
							} else if strings.Contains(line, " COPY ") {
								fmt.Fprintf(serverConn, "%s %s [COPYUID%s] completed\r\n", tag, status, code)
								continue
							}
							io.WriteString(serverConn, tag+" OK completed\r\n")
						}
					}()
					client := imapclient.New(clientConn, nil)
					t.Cleanup(func() { client.Close() })
					if err := client.WaitGreeting(); err != nil {
						t.Fatal(err)
					}
					var mapping imap.UIDMapping
					var uidValidity uint32
					var err error
					if command == "COPY" {
						var data *imap.CopyData
						data, err = client.Copy(imap.UIDSetNum(1, 2, 3, 4), "Archive").Wait()
						mapping = data.UIDMapping
						uidValidity = data.UIDValidity
					} else {
						var data *imapclient.MoveData
						data, err = client.Move(imap.UIDSetNum(1, 2, 3, 4), "Archive").Wait()
						if data != nil {
							mapping = data.UIDMapping
							uidValidity = data.UIDValidity
						}
					}
					if status == "OK" {
						if err != nil {
							t.Fatal(err)
						}
					} else {
						var imapErr *imap.Error
						if !errors.As(err, &imapErr) || imapErr.Type != imap.StatusResponseType(status) || imapErr.Text != "completed" {
							t.Fatalf("lost tagged %s status/text: %v", status, err)
						}
					}
					var got, want [][2]imap.UID
					if strings.Contains(code, "4,1:3") && (status == "OK" || command == "COPY") {
						want = [][2]imap.UID{{4, 101}, {1, 102}, {2, 110}, {3, 111}}
					}
					for source, dest := range mapping.All() {
						got = append(got, [2]imap.UID{source, dest})
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("pairs=%v, want=%v", got, want)
					}
					if (uidValidity == 7) != (len(want) != 0) || (len(want) == 0 && uidValidity != 0) {
						t.Fatalf("UIDValidity=%d with pairs=%v", uidValidity, got)
					}
					if err := client.Noop().Wait(); err != nil {
						t.Fatalf("response stream broken: %v", err)
					}
				})
			}
		}
	}
}
