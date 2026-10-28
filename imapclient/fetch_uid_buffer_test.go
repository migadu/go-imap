package imapclient_test

import (
	"bufio"
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

// A server may put UID anywhere in a FETCH response, including after body
// data. The command must receive that data through both consumption APIs.
func TestUIDFetchBufferBodyBeforeUID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attr   string
		body   string
		binary bool
	}{
		{name: "literal", attr: "BODY[] {7}\r\npayload", body: "payload"},
		{name: "quoted", attr: `BODY[] "quoted \"body\""`, body: `quoted "body"`},
		{name: "literal8", attr: "BINARY[] ~{5}\r\na\x00b\r\n", body: "a\x00b\r\n", binary: true},
	} {
		for _, collect := range []bool{true, false} {
			mode := "Next"
			if collect {
				mode = "Collect"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				client := newUIDFetchWireClient(t, nil, func(conn net.Conn, reader *bufio.Reader) error {
					tag, err := readUIDFetchWireCommand(reader)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(conn, "* 7 FETCH (%s UID 42)\r\n%s OK done\r\n", tc.attr, tag)
					return err
				})
				options := &imap.FetchOptions{}
				if tc.binary {
					options.BinarySection = []*imap.FetchItemBinarySection{{}}
				} else {
					options.BodySection = []*imap.FetchItemBodySection{{}}
				}
				cmd := client.Fetch(imap.UIDSetNum(42), options)
				if collect {
					messages, err := cmd.Collect()
					if err != nil {
						t.Fatalf("Collect() = %v", err)
					}
					if len(messages) != 1 {
						t.Fatalf("got %d messages, want 1", len(messages))
					}
					msg := messages[0]
					if msg.UID != 42 || msg.SeqNum != 7 {
						t.Errorf("message identity = seq %d, UID %d; want seq 7, UID 42", msg.SeqNum, msg.UID)
					}
					body := msg.FindBodySection(&imap.FetchItemBodySection{})
					if tc.binary {
						body = msg.FindBinarySection(&imap.FetchItemBinarySection{})
					}
					if string(body) != tc.body {
						t.Errorf("body = %q, want %q", body, tc.body)
					}
					return
				}

				defer cmd.Close()
				msg := cmd.Next()
				if msg == nil {
					t.Fatalf("Next() = nil: %v", cmd.Close())
				}
				var uid imap.UID
				var bodies []string
				for item := msg.Next(); item != nil; item = msg.Next() {
					var literal imap.LiteralReader
					switch item := item.(type) {
					case imapclient.FetchItemDataUID:
						uid = item.UID
					case imapclient.FetchItemDataBodySection:
						if tc.binary {
							t.Error("got BODY item for BINARY response")
						}
						literal = item.Literal
					case imapclient.FetchItemDataBinarySection:
						if !tc.binary {
							t.Error("got BINARY item for BODY response")
						}
						literal = item.Literal
					}
					if literal != nil {
						if literal.Size() != int64(len(tc.body)) {
							t.Errorf("literal size = %d, want %d", literal.Size(), len(tc.body))
						}
						body, err := io.ReadAll(literal)
						if err != nil {
							t.Errorf("reading body: %v", err)
						}
						bodies = append(bodies, string(body))
					}
				}
				if uid != 42 || msg.SeqNum != 7 || !reflect.DeepEqual(bodies, []string{tc.body}) {
					t.Errorf("got seq %d, UID %d, bodies %q; want seq 7, UID 42, body %q", msg.SeqNum, uid, bodies, tc.body)
				}
				if next := cmd.Next(); next != nil {
					t.Error("got an extra message")
				}
				if err := cmd.Close(); err != nil {
					t.Errorf("Close() = %v", err)
				}
			})
		}
	}
}

func TestUIDFetchBufferMultipleSectionsBeforeUID(t *testing.T) {
	client := newUIDFetchWireClient(t, nil, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, "* 1 FETCH (BODY[1] {3}\r\none BODY[2] \"two\" BINARY[3] ~{3}\r\n\x00\x01\x02 UID 42 BODY[4] {4}\r\nfour)\r\n%s OK done\r\n", tag)
		return err
	})
	sections := []*imap.FetchItemBodySection{{Part: []int{1}}, {Part: []int{2}}, {Part: []int{4}}}
	binary := &imap.FetchItemBinarySection{Part: []int{3}}
	messages, err := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{
		BodySection: sections, BinarySection: []*imap.FetchItemBinarySection{binary},
	}).Collect()
	if err != nil {
		t.Fatalf("Collect() = %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}
	msg := messages[0]
	if msg.UID != 42 || len(msg.BodySection) != 3 || len(msg.BinarySection) != 1 {
		t.Fatalf("got UID %d, %d body sections, %d binary sections", msg.UID, len(msg.BodySection), len(msg.BinarySection))
	}
	for i, want := range []string{"one", "two", "four"} {
		if got := string(msg.FindBodySection(sections[i])); got != want {
			t.Errorf("BODY[%d] = %q, want %q", sections[i].Part[0], got, want)
		}
	}
	if got := string(msg.FindBinarySection(binary)); got != "\x00\x01\x02" {
		t.Errorf("BINARY[3] = %q, want three binary octets", got)
	}
}

// More attributes than the delivery channel can hold must not force a routing
// decision before UID. Use distinct sections so every returned item is checked.
func TestUIDFetchBufferManyAttributesBeforeUID(t *testing.T) {
	const count = 40
	for _, metadata := range []bool{false, true} {
		name := "empty bodies"
		if metadata {
			name = "metadata"
		}
		t.Run(name, func(t *testing.T) {
			var response strings.Builder
			response.WriteString("* 1 FETCH (")
			options := &imap.FetchOptions{}
			for i := 1; i <= count; i++ {
				if metadata {
					fmt.Fprintf(&response, "BINARY.SIZE[%d] %d ", i, i*10)
					options.BinarySectionSize = append(options.BinarySectionSize, &imap.FetchItemBinarySectionSize{Part: []int{i}})
				} else {
					fmt.Fprintf(&response, "BODY[%d] {0}\r\n ", i)
					options.BodySection = append(options.BodySection, &imap.FetchItemBodySection{Part: []int{i}})
				}
			}
			response.WriteString("UID 42)\r\n")
			client := newUIDFetchWireClient(t, nil, func(conn net.Conn, reader *bufio.Reader) error {
				tag, err := readUIDFetchWireCommand(reader)
				if err != nil {
					return err
				}
				_, err = fmt.Fprint(conn, response.String()+tag+" OK done\r\n")
				return err
			})
			messages, err := client.Fetch(imap.UIDSetNum(42), options).Collect()
			if err != nil {
				t.Fatalf("Collect() = %v", err)
			}
			if len(messages) != 1 {
				t.Fatalf("got %d messages, want 1", len(messages))
			}
			msg := messages[0]
			if msg.UID != 42 {
				t.Errorf("UID = %d, want 42", msg.UID)
			}
			if metadata {
				if len(msg.BinarySectionSize) != count {
					t.Errorf("got %d sizes, want %d", len(msg.BinarySectionSize), count)
				}
				for i := 1; i <= count; i++ {
					if got, ok := msg.FindBinarySectionSize([]int{i}); !ok || got != uint32(i*10) {
						t.Errorf("BINARY.SIZE[%d] = %d, present %v; want %d", i, got, ok, i*10)
					}
				}
			} else {
				if len(msg.BodySection) != count {
					t.Errorf("got %d bodies, want %d", len(msg.BodySection), count)
				}
				for _, section := range options.BodySection {
					if got := msg.FindBodySection(section); got == nil || len(got) != 0 {
						t.Errorf("BODY[%d] = %#v, want present empty body", section.Part[0], got)
					}
				}
			}
		})
	}
}

func TestUIDFetchBufferConcurrentRouting(t *testing.T) {
	type unsolicitedResult struct {
		msg *imapclient.FetchMessageBuffer
		err error
	}
	unsolicited := make(chan unsolicitedResult, 3)
	client := newUIDFetchWireClient(t, &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Fetch: func(msg *imapclient.FetchMessageData) {
				buf, err := msg.Collect()
				unsolicited <- unsolicitedResult{buf, err}
			},
		},
	}, func(conn net.Conn, reader *bufio.Reader) error {
		first, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		second, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(conn, "* 9 FETCH (BODY[] {5}\r\nother UID 99)\r\n* 2 FETCH (BODY[] {6}\r\nsecond UID 22)\r\n%s OK second done\r\n* 1 FETCH (BODY[] {5}\r\nfirst UID 11)\r\n%s OK first done\r\n", second, first)
		return err
	})
	options := &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}
	first := client.Fetch(imap.UIDSetNum(11), options)
	second := client.Fetch(imap.UIDSetNum(22), options)
	for _, tc := range []struct {
		cmd  *imapclient.FetchCommand
		uid  imap.UID
		body string
	}{{second, 22, "second"}, {first, 11, "first"}} {
		messages, err := tc.cmd.Collect()
		if err != nil {
			t.Fatalf("UID %d Collect() = %v", tc.uid, err)
		}
		if len(messages) != 1 {
			t.Errorf("UID %d got %d messages, want 1", tc.uid, len(messages))
			continue
		}
		if msg := messages[0]; msg.UID != tc.uid || string(msg.FindBodySection(options.BodySection[0])) != tc.body {
			t.Errorf("UID %d got UID %d, body %q", tc.uid, msg.UID, msg.FindBodySection(options.BodySection[0]))
		}
	}
	select {
	case result := <-unsolicited:
		if result.err != nil {
			t.Fatalf("unsolicited Collect() = %v", result.err)
		}
		if msg := result.msg; msg.UID != 99 || msg.SeqNum != 9 || string(msg.FindBodySection(options.BodySection[0])) != "other" {
			t.Errorf("unsolicited message = %+v, want seq 9, UID 99, body other", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unmatched UID did not reach the unilateral handler")
	}
}

// With UID first, the caller must be able to read the literal's prefix before
// the server sends its remainder. Buffering the whole literal would deadlock.
func TestUIDFetchBufferUIDFirstStillStreams(t *testing.T) {
	releaseRemainder := make(chan struct{})
	client := newUIDFetchWireClient(t, &imapclient.Options{MaxFetchBufferSize: 1}, func(conn net.Conn, reader *bufio.Reader) error {
		tag, err := readUIDFetchWireCommand(reader)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(conn, "* 1 FETCH (UID 42 BODY[] {6}\r\nabc"); err != nil {
			return err
		}
		select {
		case <-releaseRemainder:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("caller did not consume the UID-first literal prefix")
		}
		_, err = fmt.Fprintf(conn, "def)\r\n%s OK done\r\n", tag)
		return err
	})
	cmd := client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
	defer cmd.Close()
	msg := cmd.Next()
	if msg == nil {
		t.Fatalf("Next() = nil: %v", cmd.Close())
	}
	var uid imap.UID
	var bodySeen bool
	for item := msg.Next(); item != nil; item = msg.Next() {
		switch item := item.(type) {
		case imapclient.FetchItemDataUID:
			uid = item.UID
		case imapclient.FetchItemDataBodySection:
			bodySeen = true
			prefix := make([]byte, 3)
			_, err := io.ReadFull(item.Literal, prefix)
			close(releaseRemainder)
			if err != nil || string(prefix) != "abc" {
				t.Errorf("literal prefix = %q, %v; want abc", prefix, err)
			}
			rest, err := io.ReadAll(item.Literal)
			if err != nil || string(rest) != "def" {
				t.Errorf("literal remainder = %q, %v; want def", rest, err)
			}
		}
	}
	if uid != 42 || !bodySeen {
		t.Errorf("got UID %d, body present %v; want UID 42 and body", uid, bodySeen)
	}
	if err := cmd.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

// SELECT's QRESYNC updates use the same FETCH parser. Delaying UID FETCH
// delivery must preserve the full FLAGS/UID/MODSEQ update in every order.
func TestUIDFetchBufferPreservesQResyncUpdates(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		mode := "SELECT alone"
		if pipeline {
			// RFC 9051 section 5.5 requires dependent commands to execute in
			// order. The server finishes SELECT before the pipelined UID FETCH.
			mode = "rev2 pipelined UID FETCH"
		}
		t.Run(mode, func(t *testing.T) {
			for _, attrs := range []string{
				`UID 42 FLAGS (\Seen) MODSEQ (9)`,
				`UID 42 MODSEQ (9) FLAGS (\Seen)`,
				`FLAGS (\Seen) UID 42 MODSEQ (9)`,
				`FLAGS (\Seen) MODSEQ (9) UID 42`,
				`MODSEQ (9) UID 42 FLAGS (\Seen)`,
				`MODSEQ (9) FLAGS (\Seen) UID 42`,
			} {
				t.Run(attrs, func(t *testing.T) {
					enabled := "QRESYNC"
					caps := []imap.Cap{imap.CapQResync}
					if pipeline {
						enabled = "IMAP4rev2 QRESYNC"
						caps = []imap.Cap{imap.CapIMAP4rev2, imap.CapQResync}
					}
					client := newUIDFetchWireClientWithGreeting(t, nil, "* PREAUTH [CAPABILITY IMAP4rev1 IMAP4rev2 ENABLE CONDSTORE QRESYNC] ready\r\n", func(conn net.Conn, reader *bufio.Reader) error {
						line, err := reader.ReadString('\n')
						if err != nil {
							return err
						}
						tag, command, _ := strings.Cut(line, " ")
						if command != "ENABLE "+enabled+"\r\n" {
							return fmt.Errorf("expected ENABLE %s, got %q", enabled, line)
						}
						if _, err := fmt.Fprintf(conn, "* ENABLED %s\r\n%s OK enabled\r\n", enabled, tag); err != nil {
							return err
						}
						line, err = reader.ReadString('\n')
						if err != nil {
							return err
						}
						tag, command, _ = strings.Cut(line, " ")
						if !strings.HasPrefix(command, "SELECT ") || !strings.Contains(command, "(QRESYNC (1 1))") {
							return fmt.Errorf("expected QRESYNC SELECT, got %q", line)
						}
						var fetchTag string
						if pipeline {
							fetchTag, err = readUIDFetchWireCommand(reader)
							if err != nil {
								return err
							}
						}
						_, err = fmt.Fprintf(conn, "* 1 FETCH (%s)\r\n* 2 FETCH (MODSEQ (10) FLAGS () UID 43)\r\n* 2 EXISTS\r\n* FLAGS (\\Seen)\r\n* OK [PERMANENTFLAGS (\\Seen)] flags\r\n* OK [UIDNEXT 44] next\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [HIGHESTMODSEQ 10] current\r\n* LIST () \"/\" INBOX\r\n%s OK [READ-WRITE] selected\r\n", attrs, tag)
						if err != nil || !pipeline {
							return err
						}
						_, err = fmt.Fprintf(conn, "* 1 FETCH (UID 42 BODY[] {4}\r\ndata)\r\n%s OK fetched\r\n", fetchTag)
						return err
					})
					if _, err := client.Enable(caps...).Wait(); err != nil {
						t.Fatal(err)
					}
					selectCmd := client.Select("INBOX", &imap.SelectOptions{QResync: &imap.QResyncData{UIDValidity: 1, ModSeq: 1}})
					var fetchCmd *imapclient.FetchCommand
					if pipeline {
						fetchCmd = client.Fetch(imap.UIDSetNum(42), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}})
						defer fetchCmd.Close()
					}
					data, err := selectCmd.Wait()
					if err != nil {
						t.Fatalf("SELECT: %v", err)
					}
					want := []imap.SelectModifiedData{
						{SeqNum: 1, UID: 42, Flags: []imap.Flag{imap.FlagSeen}, ModSeq: 9},
						{SeqNum: 2, UID: 43, ModSeq: 10},
					}
					if !reflect.DeepEqual(data.Modified, want) {
						t.Errorf("Modified = %+v, want %+v", data.Modified, want)
					}
					if fetchCmd != nil {
						messages, err := fetchCmd.Collect()
						if err != nil || len(messages) != 1 {
							t.Fatalf("UID FETCH returned %d messages, %v", len(messages), err)
						}
						if got := messages[0]; got.UID != 42 || string(got.FindBodySection(&imap.FetchItemBodySection{})) != "data" {
							t.Errorf("UID FETCH got UID %d, body %q; want UID 42, body data", got.UID, got.FindBodySection(&imap.FetchItemBodySection{}))
						}
					}
				})
			}
		})
	}
}

// SELECT metadata is staged even without a UID FETCH in flight, and even
// after UID has arrived. Its literal payloads must share the same byte budget.
func TestUIDFetchBufferSelectMetadataLimit(t *testing.T) {
	for _, uidFirst := range []bool{false, true} {
		for _, limit := range []int64{6, 7} {
			t.Run(fmt.Sprintf("UID first %v/limit %d", uidFirst, limit), func(t *testing.T) {
				client := newUIDFetchWireClientWithGreeting(t, &imapclient.Options{MaxFetchBufferSize: limit}, "* PREAUTH [CAPABILITY IMAP4rev2 ENABLE CONDSTORE QRESYNC] ready\r\n", func(conn net.Conn, reader *bufio.Reader) error {
					line, err := reader.ReadString('\n')
					if err != nil {
						return err
					}
					tag, command, _ := strings.Cut(line, " ")
					if command != "ENABLE QRESYNC\r\n" {
						return fmt.Errorf("expected ENABLE QRESYNC, got %q", line)
					}
					if _, err := fmt.Fprintf(conn, "* ENABLED QRESYNC\r\n%s OK enabled\r\n", tag); err != nil {
						return err
					}
					line, err = reader.ReadString('\n')
					if err != nil {
						return err
					}
					tag, command, _ = strings.Cut(line, " ")
					if !strings.HasPrefix(command, "SELECT ") {
						return fmt.Errorf("expected SELECT, got %q", line)
					}
					attrs := "ENVELOPE (NIL {3}\r\none NIL NIL NIL NIL NIL NIL NIL NIL) BODYSTRUCTURE ({4}\r\nTEXT \"PLAIN\" NIL NIL NIL \"7BIT\" 0 0)"
					if uidFirst {
						attrs = "UID 42 " + attrs
					} else {
						attrs += " UID 42"
					}
					_, err = fmt.Fprintf(conn, "* 1 FETCH (%s MODSEQ (9) FLAGS (\\Seen))\r\n* 1 EXISTS\r\n* FLAGS (\\Seen)\r\n* OK [PERMANENTFLAGS (\\Seen)] flags\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [UIDNEXT 43] next\r\n* OK [HIGHESTMODSEQ 9] current\r\n* LIST () \"/\" INBOX\r\n%s OK [READ-WRITE] selected\r\n", attrs, tag)
					if limit < 7 {
						return nil // The limit rejection may interrupt this write.
					}
					return err
				})
				if _, err := client.Enable(imap.CapQResync).Wait(); err != nil {
					t.Fatal(err)
				}
				data, err := client.Select("INBOX", &imap.SelectOptions{QResync: &imap.QResyncData{UIDValidity: 1, ModSeq: 1}}).Wait()
				if limit < 7 {
					if err == nil || !strings.Contains(err.Error(), "MaxFetchBufferSize") {
						t.Fatalf("SELECT error = %v, want MaxFetchBufferSize rejection", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := []imap.SelectModifiedData{{SeqNum: 1, UID: 42, Flags: []imap.Flag{imap.FlagSeen}, ModSeq: 9}}
				if !reflect.DeepEqual(data.Modified, want) {
					t.Errorf("Modified = %+v, want %+v", data.Modified, want)
				}
			})
		}
	}
}

func newUIDFetchWireClient(t *testing.T, options *imapclient.Options, script func(net.Conn, *bufio.Reader) error) *imapclient.Client {
	t.Helper()
	return newUIDFetchWireClientWithGreeting(t, options, "* OK [CAPABILITY IMAP4rev1 BINARY ENABLE CONDSTORE QRESYNC] ready\r\n", script)
}

func newUIDFetchWireClientWithGreeting(t *testing.T, options *imapclient.Options, greeting string, script func(net.Conn, *bufio.Reader) error) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	if err := serverConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, greeting); err != nil {
			serverDone <- err
			return
		}
		serverDone <- script(serverConn, bufio.NewReader(serverConn))
	}()
	var clientOptions imapclient.Options
	if options != nil {
		clientOptions = *options
	}
	clientOptions.ResponseTimeout = 5 * time.Second
	clientOptions.LiteralReadTimeout = 5 * time.Second
	client := imapclient.New(clientConn, &clientOptions)
	t.Cleanup(func() {
		go client.Close()
		clientConn.Close()
		serverConn.Close()
		select {
		case err := <-serverDone:
			if err != nil && !t.Failed() {
				t.Errorf("wire server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("wire server did not stop")
		}
		select {
		case <-client.Closed():
		case <-time.After(5 * time.Second):
			t.Error("client response reader did not stop")
		}
	})
	return client
}

func readUIDFetchWireCommand(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	tag, command, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(command, "UID FETCH ") {
		return "", fmt.Errorf("expected UID FETCH, got %q", line)
	}
	return tag, nil
}
