package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type appendOptionsSession struct {
	imapserver.Session
	got chan imap.AppendOptions
}

func (s *appendOptionsSession) Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	s.got <- *options
	return s.Session.Append(ctx, mailbox, r, options)
}

// RFC 3516 §4.4: "If the destination mailbox does not support the storage of
// binary content, the server MUST fail the request and issue a "NO" response
// that contains the "UNKNOWN-CTE" extended response code." Only the backend
// knows that, so it has to be told the message came as a literal8.
func TestAppendBinaryReachesSession(t *testing.T) {
	got := make(chan imap.AppendOptions, 1)
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapBinary: {}}, func(sess imapserver.Session) imapserver.Session {
		return &appendOptionsSession{Session: sess, got: got}
	})

	for _, tc := range []struct {
		literal string
		binary  bool
	}{
		{"~{5+}\r\nab\x00cd", true},
		{"{5+}\r\nabxcd", false},
	} {
		if _, tagged := rc.try("APPEND INBOX " + tc.literal); !strings.Contains(tagged, " OK ") {
			t.Fatalf("APPEND %q → %q", tc.literal, tagged)
		}
		if options := <-got; options.Binary != tc.binary {
			t.Errorf("APPEND %q: options.Binary = %v, want %v", tc.literal, options.Binary, tc.binary)
		}
	}
}

// RFC 9051 Appendix B: IMAP4rev2 has BINARY's FETCH items "but not extensions
// to the APPEND command". The literal8 APPEND of RFC 3516 §4.4 is BINARY's
// alone, so a server that does not offer BINARY answers it BAD, as a command
// it does not implement (RFC 9051 §7.1.3), and does not ask for the data. The
// octets of a non-synchronizing literal are read through. Issue #169.
func TestAppendBinaryNeedsBinaryCapability(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps imap.CapSet
	}{
		{"IMAP4rev1", imap.CapSet{imap.CapIMAP4rev1: {}}},
		{"IMAP4rev2", imap.CapSet{imap.CapIMAP4rev2: {}}},
		{"IMAP4rev1 and IMAP4rev2", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newSearchRev2Conn(t, tc.caps)

			if _, tagged := rc.try("APPEND INBOX ~{5+}\r\nab\x00cd"); !strings.Contains(tagged, " BAD ") {
				t.Errorf("APPEND ~{5+} → %q, want BAD", strings.TrimRight(tagged, "\r\n"))
			}
			rc.do("NOOP")

			// A synchronizing literal8 gets no continuation request.
			if _, err := rc.c.Write([]byte("s1 APPEND INBOX ~{5}\r\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
			line, err := rc.br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading the answer to APPEND ~{5}: %v", err)
			}
			if !strings.HasPrefix(line, "s1 BAD ") {
				t.Fatalf("APPEND ~{5} → %q, want s1 BAD", strings.TrimRight(line, "\r\n"))
			}
			rc.do("NOOP")

			if resp := rc.do("STATUS INBOX (MESSAGES)"); !strings.Contains(resp, "(MESSAGES 0)") {
				t.Errorf("STATUS after the refused APPENDs → %q, want no message stored", resp)
			}
		})
	}
}

// Per-session capabilities override Options.Caps for both advertisement and
// command acceptance. Retain rev2 support through the wrapper as well.
type appendBinaryCapabilitiesSession struct {
	imapserver.SessionIMAP4rev2
	caps imap.CapSet
	got  chan imap.AppendOptions
}

func (s *appendBinaryCapabilitiesSession) GetCapabilities() imap.CapSet { return s.caps }

func (s *appendBinaryCapabilitiesSession) Append(ctx context.Context, mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	s.got <- *options
	return s.SessionIMAP4rev2.Append(ctx, mailbox, r, options)
}

func TestAppendBinarySessionCapabilityOverride(t *testing.T) {
	for _, protocols := range []imap.CapSet{
		{imap.CapIMAP4rev1: {}},
		{imap.CapIMAP4rev2: {}},
		{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
	} {
		for _, allowed := range []bool{false, true} {
			name := "session refuses BINARY"
			if allowed {
				name = "session offers BINARY"
			}
			t.Run(fmt.Sprintf("%v/%s", protocols, name), func(t *testing.T) {
				serverCaps, sessionCaps := protocols.Copy(), protocols.Copy()
				if allowed {
					sessionCaps[imap.CapBinary] = struct{}{}
				} else {
					serverCaps[imap.CapBinary] = struct{}{}
				}
				got := make(chan imap.AppendOptions, 2)
				rc := newSearchRev2ConnWrapped(t, serverCaps, func(sess imapserver.Session) imapserver.Session {
					return &appendBinaryCapabilitiesSession{sess.(imapserver.SessionIMAP4rev2), sessionCaps, got}
				})
				if resp := rc.do("CAPABILITY"); strings.Contains(resp, " BINARY") != allowed {
					t.Fatalf("CAPABILITY = %q, want BINARY offered=%v", resp, allowed)
				}

				_, nonSync := rc.try("APPEND INBOX ~{5+}\r\nab\x00cd")
				sync, continuations := rc.exchange("APPEND INBOX ~{5}", "ab\x00cd")
				wantStatus, wantContinuations, wantMessages := " BAD ", 0, "(MESSAGES 0)"
				if allowed {
					wantStatus, wantContinuations, wantMessages = " OK ", 1, "(MESSAGES 2)"
				}
				if !strings.Contains(nonSync, wantStatus) || !strings.Contains(sync, wantStatus) || continuations != wantContinuations {
					t.Fatalf("non-sync=%q, sync=%q, continuations=%d; want status %s and %d continuations", nonSync, sync, continuations, wantStatus, wantContinuations)
				}
				if allowed {
					for i := 0; i < 2; i++ {
						if options := <-got; !options.Binary {
							t.Error("a permitted literal8 did not reach the session as Binary")
						}
					}
				} else if len(got) != 0 {
					t.Error("a refused literal8 reached Session.Append")
				}
				rc.do("NOOP")
				if resp := rc.do("STATUS INBOX (MESSAGES)"); !strings.Contains(resp, wantMessages) {
					t.Errorf("STATUS = %q, want %s", resp, wantMessages)
				}
			})
		}
	}
}

func TestAppendUTF8LiteralDoesNotRequireBinary(t *testing.T) {
	got := make(chan imap.AppendOptions, 1)
	rc := newSearchRev2ConnWrapped(t, imap.CapSet{imap.CapIMAP4rev1: {}}, func(sess imapserver.Session) imapserver.Session {
		return &appendOptionsSession{Session: sess, got: got}
	})
	rc.do("ENABLE UTF8=ACCEPT")
	rc.do("APPEND INBOX UTF8 (~{5+}\r\nhello)")
	if options := <-got; options.Binary {
		t.Error("the UTF8 wrapper was treated as a bare BINARY literal8")
	}
	rc.do("NOOP")
}
