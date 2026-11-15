package imapserver_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapclient"
	"github.com/migadu/go-imap/imapserver"
	"github.com/migadu/go-imap/imapserver/imapmemserver"
)

// Non-ASCII letters that Unicode case mapping turns into the ASCII letters of
// a keyword: strings.ToUpper maps U+017F to "S" and U+0131 to "I", and
// strings.EqualFold matches U+017F with "s" and U+212A with "k".
const (
	longS    = "ſ" // LATIN SMALL LETTER LONG S
	dotlessI = "ı" // LATIN SMALL LETTER DOTLESS I
	dottedI  = "İ" // LATIN CAPITAL LETTER I WITH DOT ABOVE
	kelvin   = "K" // KELVIN SIGN
)

var asciiKeywordCaps = imap.CapSet{
	imap.CapIMAP4rev1:    {},
	imap.CapEnable:       {},
	imap.CapCondStore:    {},
	imap.CapESearch:      {},
	imap.CapSort:         {},
	imap.CapESort:        {},
	imap.CapListExtended: {},
}

// "ABNF strings are case insensitive and the character set for these strings
// is US-ASCII" (RFC 5234 §2.3), and RFC 9051 §9 defines its keywords in ABNF.
// A token with a non-ASCII letter in it is none of them, however it maps under
// Unicode case rules.
func TestKeywordLookalikesBeforeAuthAreUnknown(t *testing.T) {
	for _, cmd := range []string{
		"CAPAB" + dotlessI + "LITY",
		"LOG" + dotlessI + "N " + searchRev2Username + " " + searchRev2Password,
		"AUTHENTICATE PLA" + dotlessI + "N",
	} {
		// Read a single line: a mechanism taken for PLAIN answers with a
		// continuation request rather than a tagged response.
		rc := dialSearchRev2Conn(t, asciiKeywordCaps, nil)
		if _, err := rc.c.Write([]byte("t1 " + cmd + "\r\n")); err != nil {
			t.Fatalf("write %q: %v", cmd, err)
		}
		line, err := rc.br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading response to %q: %v", cmd, err)
		}
		if !strings.HasPrefix(line, "t1 BAD ") && !strings.HasPrefix(line, "t1 NO ") {
			t.Errorf("%s → %s, want it refused", cmd, strings.TrimRight(line, "\r\n"))
		}
	}

	// Case-insensitive ASCII is still the command.
	rc := dialSearchRev2Conn(t, asciiKeywordCaps, nil)
	if resp := rc.do("capability"); !strings.Contains(resp, "* CAPABILITY ") {
		t.Errorf("capability: want a CAPABILITY response, got:\n%s", resp)
	}
}

func TestKeywordLookalikesAreNotKeywords(t *testing.T) {
	rc := newSearchRev2Conn(t, asciiKeywordCaps)
	rc.appendOne("sort")
	rc.do("SELECT INBOX")

	const refused = "" // BAD or NO, with no untagged response
	for _, tc := range []struct{ cmd, want string }{
		// Command names and the UID sub-command (Conn.readCommand).
		{longS + "ELECT INBOX", " BAD "},
		{longS + "TATUS INBOX (MESSAGES)", " BAD "},
		{"U" + dotlessI + "D SEARCH ALL", " BAD "},
		{"UID " + longS + "EARCH ALL", " BAD "},
		{"UID " + longS + "TORE 1 +FLAGS (\\Seen)", " BAD "},
		{"CAPAB" + dotlessI + "LITY", " BAD "},

		// STATUS items.
		{"STATUS INBOX (MESSAGE" + longS + ")", " BAD "},
		{"STATUS INBOX (U" + dotlessI + "DNEXT)", " BAD "},

		// FETCH items, macros and section specifiers; STORE items.
		{"FETCH 1 (FLAG" + longS + ")", " BAD "},
		{"FETCH 1 " + dotlessI + "NTERNALDATE", " BAD "},
		{"FETCH 1 RFC822." + longS + "IZE", " BAD "},
		{"FETCH 1 FA" + longS + "T", " BAD "},
		{"FETCH 1 BODY.PEEK[HEADER.F" + dotlessI + "ELDS (Subject)]", " BAD "},
		{"STORE 1 +FLAG" + longS + " (\\Seen)", " BAD "},

		// Search keys, CHARSET and RETURN options.
		{"SEARCH " + longS + "UBJECT sort", " BAD "},
		{"SEARCH UN" + longS + "EEN", " BAD "},
		{"SEARCH NOT " + longS + "EEN", " BAD "},
		{"SEARCH " + kelvin + "EYWORD foo", " BAD "},
		{"SEARCH CHAR" + longS + "ET UTF-8 ALL", " BAD "},
		{"SEARCH RETURN (M" + dotlessI + "N) ALL", " BAD "},
		{"SEARCH CHARSET U" + longS + "-A" + longS + "C" + dotlessI + "I ALL", " NO [BADCHARSET] "},

		// SORT keys, REVERSE, RETURN options and charset.
		{"SORT (" + longS + "IZE) UTF-8 ALL", refused},
		{"SORT (REVER" + longS + "E DATE) UTF-8 ALL", refused},
		{"SORT RETURN (M" + dotlessI + "N) (DATE) UTF-8 ALL", " BAD "},
		{"SORT (DATE) U" + longS + "-A" + longS + "C" + dotlessI + "I ALL", " NO [BADCHARSET] "},

		// LIST selection and return options.
		{"LIST (" + longS + "UBSCRIBED) \"\" \"*\"", " BAD "},
		{"LIST \"\" \"*\" RETURN (" + longS + "UBSCRIBED)", " BAD "},

		// Last: on success it would leave the mailbox read-only.
		{"EXAM" + dotlessI + "NE INBOX", " BAD "},
	} {
		resp, tagged := rc.try(tc.cmd)
		switch {
		case tc.want == refused && strings.Contains(tagged, " OK "),
			tc.want != refused && !strings.Contains(tagged, tc.want):
			want := tc.want
			if want == refused {
				want = "BAD or NO"
			}
			t.Errorf("%s → %s, want %q", tc.cmd, strings.TrimRight(tagged, "\r\n"), want)
		case resp != tagged:
			t.Errorf("%s: want no untagged response, got:\n%s", tc.cmd, resp)
		}
		rc.requireInSync(tc.cmd)
	}

	// The same keywords in ASCII, in any case, are served.
	for _, tc := range []struct{ cmd, want string }{
		{"status INBOX (messages uidnext)", "* STATUS INBOX (MESSAGES 1 UIDNEXT "},
		{"uid search all", "* SEARCH 1"},
		{"fetch 1 (flags rfc822.size internaldate)", "* 1 FETCH ("},
		{"fetch 1 body.peek[header.fields (Subject)]", `BODY[HEADER.FIELDS ("Subject")]`},
		{"search charset us-ascii subject sort", "* SEARCH 1"},
		{"search return (min) all", " MIN 1"},
		{"sort (reverse size) us-ascii all", "* SORT 1"},
		{"list (subscribed) \"\" \"*\"", " OK "},
	} {
		if resp := rc.do(tc.cmd); !strings.Contains(resp, tc.want) {
			t.Errorf("%s: want %q, got:\n%s", tc.cmd, tc.want, resp)
		}
	}
}

// ENABLE names capabilities, which are atoms compared without regard to ASCII
// case (RFC 5161 §3.1, RFC 9051 §9). One spelled with a look-alike names no
// capability the server knows, so it is not enabled.
func TestEnableLookalikeCapabilityIsNotEnabled(t *testing.T) {
	rc := newSearchRev2Conn(t, asciiKeywordCaps)
	if resp := rc.do("ENABLE COND" + longS + "TORE"); strings.Contains(resp, "CONDSTORE") {
		t.Errorf("ENABLE COND%sTORE enabled CONDSTORE:\n%s", longS, resp)
	}
	if resp := rc.do("enable condstore"); !strings.Contains(resp, "* ENABLED CONDSTORE") {
		t.Errorf("enable condstore: want CONDSTORE enabled, got:\n%s", resp)
	}
}

// HEADER.FIELDS lists header field names (RFC 9051 §6.4.5), which are
// printable US-ASCII (RFC 5322 §2.2) compared without regard to ASCII case.
// strings.ToLower made a field named with U+212A or U+0130 match one spelled
// with "k" or "i"; HEADER.FIELDS.NOT already removed ASCII case variants only.
func TestExtractBodySectionHeaderFieldsLookalikes(t *testing.T) {
	const msg = "Keywords: a\r\nMIME-Version: 1.0\r\nSubject: x\r\n\r\nbody\r\n"
	for _, tc := range []struct {
		fields []string
		want   string
	}{
		{[]string{kelvin + "eywords", "MİME-Version"}, "\r\n"},
		{[]string{"KEYWORDS", "mime-version"}, "Keywords: a\r\nMIME-Version: 1.0\r\n\r\n"},
	} {
		got := imapserver.ExtractBodySection(strings.NewReader(msg), &imap.FetchItemBodySection{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: tc.fields,
		})
		if string(got) != tc.want {
			t.Errorf("HEADER.FIELDS %q = %q, want %q", tc.fields, got, tc.want)
		}
	}
}

type echoIDSession struct {
	imapserver.Session
}

func (s *echoIDSession) ID(clientID *imap.IDData) *imap.IDData {
	return clientID
}

func dialASCIIKeywordClient(t *testing.T, caps imap.CapSet, wrap func(imapserver.Session) imapserver.Session) *imapclient.Client {
	t.Helper()
	memUser := imapmemserver.NewUser(searchRev2Username, searchRev2Password)
	if err := memUser.Create(context.Background(), "INBOX", nil); err != nil {
		t.Fatalf("Create INBOX: %v", err)
	}
	memServer := imapmemserver.New()
	memServer.AddUser(memUser)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			var sess imapserver.Session = memServer.NewSession()
			if wrap != nil {
				sess = wrap(sess)
			}
			return sess, nil, nil
		},
		Caps:         caps,
		InsecureAuth: true,
	})
	t.Cleanup(func() { srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go srv.Serve(ln)

	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	if err := c.Login(searchRev2Username, searchRev2Password).Wait(); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return c
}

// readID lower-cases ASCII letters only, so a look-alike such as "versİon"
// (U+0130) lands in IDData.Raw without populating IDData.Version. Both
// imapserver and imapclient must filter and parse ID keys with ASCII case
// folding as well, rather than dropping Raw["versİon"] or folding it onto
// Version.
func TestIDLookalikeKeysPreserved(t *testing.T) {
	caps := imap.CapSet{
		imap.CapIMAP4rev1:  {},
		imap.CapEnable:     {},
		imap.CapUTF8Accept: {},
		imap.CapID:         {},
	}
	lookalikeKey := "vers" + dottedI + "on"

	t.Run("server_echo", func(t *testing.T) {
		rc := newSearchRev2ConnWrapped(t, caps, func(sess imapserver.Session) imapserver.Session {
			return &echoIDSession{Session: sess}
		})
		rc.do("ENABLE UTF8=ACCEPT")

		resp := rc.do(`ID ("` + lookalikeKey + `" "1.0")`)
		if strings.Contains(resp, "* ID ()") {
			t.Fatalf("ID response emitted invalid empty list and dropped key: %q", resp)
		}
		if !strings.Contains(resp, `"`+lookalikeKey+`" "1.0"`) {
			t.Fatalf("ID response missing Raw look-alike key, got: %q", resp)
		}
	})

	t.Run("client_send", func(t *testing.T) {
		var gotClientID *imap.IDData
		c := dialASCIIKeywordClient(t, caps, func(sess imapserver.Session) imapserver.Session {
			return &captureIDSession{Session: sess, onID: func(id *imap.IDData) { gotClientID = id }}
		})
		if _, err := c.Enable(imap.CapUTF8Accept).Wait(); err != nil {
			t.Fatalf("Enable UTF8=ACCEPT: %v", err)
		}
		if _, err := c.ID(&imap.IDData{
			Raw: map[string]string{lookalikeKey: "1.0"},
		}).Wait(); err != nil {
			t.Fatalf("Client.ID: %v", err)
		}
		if gotClientID == nil || gotClientID.Raw[lookalikeKey] != "1.0" {
			t.Fatalf("server received client ID %+v, want Raw[%q]=1.0", gotClientID, lookalikeKey)
		}
	})

	t.Run("client_recv", func(t *testing.T) {
		sConn, cConn := net.Pipe()
		t.Cleanup(func() {
			sConn.Close()
			cConn.Close()
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = sConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 ID UTF8=ACCEPT] ready\r\n"))
			buf := make([]byte, 256)
			n, _ := sConn.Read(buf)
			tag := strings.SplitN(string(buf[:n]), " ", 2)[0]
			_, _ = sConn.Write([]byte("* ID (\"" + lookalikeKey + "\" \"1.0\")\r\n" + tag + " OK ID completed\r\n"))
		}()

		c := imapclient.New(cConn, nil)
		t.Cleanup(func() { c.Close() })
		got, err := c.ID(nil).Wait()
		<-done
		if err != nil {
			t.Fatalf("Client.ID: %v", err)
		}
		if got.Version != "" {
			t.Errorf("IDData.Version = %q, want empty for look-alike key %q", got.Version, lookalikeKey)
		}
		if got.Raw[lookalikeKey] != "1.0" {
			t.Errorf("IDData.Raw[%q] = %q, want %q (Raw=%v)", lookalikeKey, got.Raw[lookalikeKey], "1.0", got.Raw)
		}
	})
}

type captureIDSession struct {
	imapserver.Session
	onID func(*imap.IDData)
}

func (s *captureIDSession) ID(clientID *imap.IDData) *imap.IDData {
	s.onID(clientID)
	return nil
}

// ExtractBodySection matches HEADER.FIELDS with ASCII case folding only, so
// imapclient's FindBodySection must not fold U+212A ("Keywords") onto
// "Keywords" when matching returned sections.
func TestClientFindBodySectionHeaderFieldsLookalikes(t *testing.T) {
	c := dialASCIIKeywordClient(t, imap.CapSet{imap.CapIMAP4rev1: {}}, nil)

	const rawMsg = "Keywords: alpha\r\nSubject: hello\r\n\r\nbody\r\n"
	appendCmd := c.Append("INBOX", int64(len(rawMsg)), nil)
	if _, err := appendCmd.Write([]byte(rawMsg)); err != nil {
		t.Fatalf("Append write: %v", err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatalf("Append close: %v", err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatalf("Append wait: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("Select: %v", err)
	}

	secLookalike := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{kelvin + "eywords"},
		Peek:         true,
	}
	secReal := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"Keywords"},
		Peek:         true,
	}

	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{secLookalike, secReal},
	}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Fetch: %d msgs, err=%v", len(msgs), err)
	}

	if got := string(msgs[0].FindBodySection(secLookalike)); got != "\r\n" {
		t.Errorf("FindBodySection(lookalike) = %q, want %q", got, "\r\n")
	}
	if got, want := string(msgs[0].FindBodySection(secReal)), "Keywords: alpha\r\n\r\n"; got != want {
		t.Errorf("FindBodySection(Keywords) = %q, want %q", got, want)
	}
}

// internal.canonicalFlag uses ASCII case folding only, so imapmemserver's own
// canonicalFlag must not fold "$İmported" (U+0130) onto "$Imported".
func TestMemServerFlagLookalikesDoNotCollide(t *testing.T) {
	rc := newSearchRev2Conn(t, imap.CapSet{
		imap.CapIMAP4rev1:  {},
		imap.CapEnable:     {},
		imap.CapUTF8Accept: {},
	})
	rc.do("ENABLE UTF8=ACCEPT")
	rc.appendOne("flag-test")
	rc.do("SELECT INBOX")

	rc.do("STORE 1 +FLAGS ($Imported)")

	if resp := rc.do("SEARCH KEYWORD $" + dottedI + "mported"); strings.Contains(resp, "* SEARCH 1") {
		t.Fatalf("SEARCH KEYWORD $%smported falsely matched $Imported:\n%s", dottedI, resp)
	}

	rc.do("STORE 1 -FLAGS ($" + dottedI + "mported)")
	if resp := rc.do("SEARCH KEYWORD $Imported"); !strings.Contains(resp, "* SEARCH 1") {
		t.Fatalf("STORE -FLAGS ($%smported) removed $Imported:\n%s", dottedI, resp)
	}
}
