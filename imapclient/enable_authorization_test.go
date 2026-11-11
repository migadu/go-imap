package imapclient

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func newEnabledAuthorizationClient(t *testing.T, greeting string, script func(net.Conn, *bufio.Reader)) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go func() {
		br := bufio.NewReader(serverConn)
		io.WriteString(serverConn, greeting+" [CAPABILITY IMAP4rev1 ENABLE LITERAL+ UNAUTHENTICATE] ready\r\n")
		script(serverConn, br)
		io.Copy(io.Discard, br)
	}()
	client := New(clientConn, &Options{ResponseTimeout: time.Second})
	t.Cleanup(func() { client.Close(); serverConn.Close() })
	return client
}

func enabledCommandTag(t *testing.T, br *bufio.Reader, want string) string {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Error(err)
		return ""
	}
	tag, command, _ := strings.Cut(strings.TrimSuffix(line, "\r\n"), " ")
	if !strings.HasPrefix(command, want) {
		t.Errorf("command = %q, want %q", command, want)
	}
	return tag
}

func TestEnabledRejectsUnrequestedSyntaxChanges(t *testing.T) {
	client := newEnabledAuthorizationClient(t, "* PREAUTH", func(conn net.Conn, br *bufio.Reader) {
		tag := enabledCommandTag(t, br, "ENABLE CONDSTORE")
		io.WriteString(conn, "* ENABLED CONDSTORE UTF8=ACCEPT IMAP4rev2\r\n"+tag+" OK enabled\r\n")
	})
	data, err := client.Enable(imap.CapCondStore).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Caps) != 1 || !data.Caps.Has(imap.CapCondStore) {
		t.Errorf("EnableData.Caps = %v, want only requested CONDSTORE", data.Caps)
	}
	if client.useQuotedUTF8() || client.dec.QuotedUTF8 {
		t.Error("unrequested ENABLED changed wire syntax")
	}
}

func TestEnabledIgnoresPendingCommandsOutsideAuthenticatedStates(t *testing.T) {
	for _, state := range []imap.ConnState{imap.ConnStateNone, imap.ConnStateNotAuthenticated, imap.ConnStateLogout} {
		cmd := &EnableCommand{requested: imap.CapSet{imap.CapUTF8Accept: {}}}
		client := &Client{
			state: state, enabled: make(imap.CapSet), pendingCmds: []command{cmd},
			dec: imapwire.NewDecoder(bufio.NewReader(strings.NewReader(" UTF8=ACCEPT\r\n")), imapwire.ConnSideClient),
		}
		if err := client.handleEnabled(); err != nil {
			t.Fatal(err)
		}
		if len(client.enabled) != 0 || len(cmd.data.Caps) != 0 || client.dec.QuotedUTF8 {
			t.Errorf("ENABLED with pending command in %v changed state", state)
		}
	}
}

func TestEnabledPipelinedRepliesCanCompleteInReverse(t *testing.T) {
	client := newEnabledAuthorizationClient(t, "* PREAUTH", func(conn net.Conn, br *bufio.Reader) {
		first := enabledCommandTag(t, br, "ENABLE CONDSTORE")
		second := enabledCommandTag(t, br, "ENABLE UTF8=ACCEPT")
		io.WriteString(conn, "* ENABLED UTF8=ACCEPT\r\n"+second+" OK second\r\n"+
			"* ENABLED CONDSTORE\r\n"+first+" OK first\r\n")
	})
	first, second := client.Enable(imap.CapCondStore), client.Enable(imap.CapUTF8Accept)
	for i, cmd := range []*EnableCommand{first, second} {
		data, err := cmd.Wait()
		want := []imap.Cap{imap.CapCondStore, imap.CapUTF8Accept}[i]
		if err != nil || len(data.Caps) != 1 || !data.Caps.Has(want) {
			t.Errorf("Enable %d = %v, %v; want only %s", i, data.Caps, err, want)
		}
	}
	if !client.useQuotedUTF8() {
		t.Error("pipelined UTF8=ACCEPT was lost")
	}
}

func TestEnabledAccumulatesAndPermitsPriorEchoes(t *testing.T) {
	client := newEnabledAuthorizationClient(t, "* PREAUTH", func(conn net.Conn, br *bufio.Reader) {
		first := enabledCommandTag(t, br, "ENABLE QRESYNC")
		io.WriteString(conn, "* ENABLED QRESYNC\r\n* ENABLED CONDSTORE\r\n"+first+" OK first\r\n")
		second := enabledCommandTag(t, br, "ENABLE UTF8=ACCEPT")
		io.WriteString(conn, "* ENABLED QRESYNC CONDSTORE UTF8=ACCEPT\r\n"+second+" OK second\r\n")
	})
	data, err := client.Enable(imap.CapQResync).Wait()
	if err != nil || !data.Caps.Has(imap.CapQResync) || !data.Caps.Has(imap.CapCondStore) {
		t.Fatalf("QRESYNC with implied CONDSTORE = %v, %v", data.Caps, err)
	}
	data, err = client.Enable(imap.CapUTF8Accept).Wait()
	if err != nil || len(data.Caps) != 3 || !data.Caps.Has(imap.CapUTF8Accept) {
		t.Errorf("ENABLE with prior echoes = %v, %v", data.Caps, err)
	}
}

func TestEnabledPipelinedLoginAndSelection(t *testing.T) {
	client := newEnabledAuthorizationClient(t, "* OK", func(conn net.Conn, br *bufio.Reader) {
		login := enabledCommandTag(t, br, "LOGIN ")
		enable := enabledCommandTag(t, br, "ENABLE IMAP4rev2")
		selection := enabledCommandTag(t, br, "SELECT ")
		io.WriteString(conn, login+" OK [CAPABILITY IMAP4rev1 IMAP4rev2 ENABLE] authenticated\r\n"+
			"* ENABLED IMAP4rev2\r\n"+enable+" OK enabled\r\n* 2 EXISTS\r\n"+selection+" OK selected\r\n")
	})
	login := client.Login("user", "pass")
	enable := client.Enable(imap.CapIMAP4rev2)
	selection := client.Select("INBOX", nil)
	if err := login.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := enable.Wait()
	if err != nil || !data.Caps.Has(imap.CapIMAP4rev2) {
		t.Errorf("pipelined ENABLE = %v, %v", data.Caps, err)
	}
	selected, err := selection.Wait()
	if err != nil || selected.NumMessages != 2 || !client.useQuotedUTF8() {
		t.Errorf("selection after pipelined ENABLE = %+v, %v", selected, err)
	}
}

func TestUnauthenticateResetsDecoderAfterEnabledUTF8(t *testing.T) {
	client := newEnabledAuthorizationClient(t, "* PREAUTH", func(conn net.Conn, br *bufio.Reader) {
		enable := enabledCommandTag(t, br, "ENABLE UTF8=ACCEPT")
		io.WriteString(conn, "* ENABLED UTF8=ACCEPT\r\n"+enable+" OK enabled\r\n")
		unauth := enabledCommandTag(t, br, "UNAUTHENTICATE")
		io.WriteString(conn, unauth+" OK [CAPABILITY IMAP4rev1] unauthenticated\r\n")
	})
	if _, err := client.Enable(imap.CapUTF8Accept).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := client.Unauthenticate().Wait(); err != nil {
		t.Fatal(err)
	}
	if client.useQuotedUTF8() || client.dec.QuotedUTF8 {
		t.Error("UNAUTHENTICATE retained enabled UTF-8 decoder syntax")
	}
	if state := client.State(); state != imap.ConnStateNotAuthenticated {
		t.Errorf("state = %v", state)
	}
}
