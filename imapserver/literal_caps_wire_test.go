package imapserver_test

import (
	"bufio"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// capTokens extracts the capability atoms from either an untagged
// "* CAPABILITY a b c" line or a status line carrying a "[CAPABILITY a b c]"
// response code, sorted.
func capTokens(t *testing.T, line string) []string {
	t.Helper()
	var list string
	switch {
	case strings.HasPrefix(line, "* CAPABILITY "):
		list = strings.TrimPrefix(line, "* CAPABILITY ")
	default:
		i := strings.Index(line, "[CAPABILITY ")
		j := strings.Index(line, "]")
		if i < 0 || j < i {
			t.Fatalf("no CAPABILITY in %q", line)
		}
		list = line[i+len("[CAPABILITY ") : j]
	}
	toks := strings.Fields(list)
	sort.Strings(toks)
	return toks
}

func hasTok(toks []string, want string) bool {
	for _, tok := range toks {
		if tok == want {
			return true
		}
	}
	return false
}

// TestLiteralCapsOnTheWire is the end-to-end form of RFC 7888 §3: across the
// greeting, the unauthenticated CAPABILITY, the LOGIN completion code and the
// authenticated CAPABILITY, LITERAL- and LITERAL+ are never listed together, and
// the two status-response codes agree with the CAPABILITY response issued in the
// same state.
func TestLiteralCapsOnTheWire(t *testing.T) {
	memUser := imapmemserver.NewUser("user", "pass")
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:   {},
			imap.CapIMAP4rev2:   {},
			imap.CapLiteralPlus: {},
		},
		InsecureAuth: true,
	})
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	readLine := func() string {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return strings.TrimRight(line, "\r\n")
	}
	cmd := func(tag, command string) (untagged []string, tagged string) {
		if _, err := conn.Write([]byte(tag + " " + command + "\r\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		for {
			line := readLine()
			if strings.HasPrefix(line, tag+" ") {
				return untagged, line
			}
			untagged = append(untagged, line)
		}
	}
	capabilityLine := func(untagged []string) string {
		for _, l := range untagged {
			if strings.HasPrefix(l, "* CAPABILITY ") {
				return l
			}
		}
		t.Fatalf("no untagged CAPABILITY in %v", untagged)
		return ""
	}

	greeting := capTokens(t, readLine())
	pre, _ := cmd("a1", "CAPABILITY")
	preAuth := capTokens(t, capabilityLine(pre))
	_, loginTagged := cmd("a2", "LOGIN user pass")
	if !strings.HasPrefix(loginTagged, "a2 OK ") {
		t.Fatalf("LOGIN: %s", loginTagged)
	}
	loginCode := capTokens(t, loginTagged)
	post, _ := cmd("a3", "CAPABILITY")
	postAuth := capTokens(t, capabilityLine(post))

	for name, toks := range map[string][]string{"greeting": greeting, "pre-auth CAPABILITY": preAuth} {
		if !hasTok(toks, "LITERAL-") || hasTok(toks, "LITERAL+") {
			t.Errorf("%s: want LITERAL- and no LITERAL+, got %v", name, toks)
		}
	}
	for name, toks := range map[string][]string{"LOGIN code": loginCode, "post-auth CAPABILITY": postAuth} {
		if !hasTok(toks, "LITERAL+") || hasTok(toks, "LITERAL-") {
			t.Errorf("%s: want LITERAL+ and no LITERAL-, got %v", name, toks)
		}
	}
	if strings.Join(greeting, " ") != strings.Join(preAuth, " ") {
		t.Errorf("greeting code %v != pre-auth CAPABILITY %v", greeting, preAuth)
	}
	if strings.Join(loginCode, " ") != strings.Join(postAuth, " ") {
		t.Errorf("LOGIN code %v != post-auth CAPABILITY %v", loginCode, postAuth)
	}
}
