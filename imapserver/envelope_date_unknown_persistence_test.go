package imapserver

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"net/mail"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

func TestEnvelopeExplicitUnknownZoneSurvivesPersistence(t *testing.T) {
	// A decoded fixed-zero location can coalesce with time.UTC when Local is
	// UTC. Keep this application configuration isolated from other tests and
	// their background goroutines, and register gob interface types only here.
	const childEnv = "GOIMAP_TEST_EXPLICIT_UNKNOWN_ZONE_PERSISTENCE"
	if os.Getenv(childEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestEnvelopeExplicitUnknownZoneSurvivesPersistence$", "-test.count=1", "-test.timeout=30s")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated explicit-zone persistence checks failed: %v\n%s", err, output)
		}
		return
	}
	time.Local = time.UTC
	gob.Register(&imap.BodyStructureSinglePart{})
	gob.Register(&imap.BodyStructureMultiPart{})
	gob.Register(&imap.BodyStructureText{})
	gob.Register(&imap.BodyStructureMessageRFC822{})

	t.Run("direct envelope gob", func(t *testing.T) {
		date := time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC)
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("unknown=%t", unknown), func(t *testing.T) {
				original := &imap.Envelope{Date: date, DateUnknownZone: unknown, Subject: "persisted"}
				var encoded bytes.Buffer
				if err := gob.NewEncoder(&encoded).Encode(original); err != nil {
					t.Fatal(err)
				}
				var restored imap.Envelope
				if err := gob.NewDecoder(&encoded).Decode(&restored); err != nil {
					t.Fatal(err)
				}
				if !restored.Date.Equal(date) || restored.DateUnknownZone != unknown {
					t.Fatalf("gob lost explicit date metadata: %+v", restored)
				}
				zone := "+0000"
				if unknown {
					zone = "-0000"
				}
				requirePersistedEnvelopeDate(t, &restored, "Tue, 01 Jan 2019 12:00:00 "+zone)
			})
		}
	})

	t.Run("legacy envelope gob has known zone by default", func(t *testing.T) {
		// This is the old wire schema: no DateUnknownZone field. A new reader
		// must retain the normal known-UTC rendering rather than guessing from
		// the decoded time's Location. Use both forms that previously collided.
		for _, location := range []*time.Location{time.UTC, time.FixedZone("known UTC", 0)} {
			legacy := struct {
				Date    time.Time
				Subject string
			}{time.Date(2019, 1, 1, 12, 0, 0, 0, location), "legacy"}
			var encoded bytes.Buffer
			if err := gob.NewEncoder(&encoded).Encode(legacy); err != nil {
				t.Fatal(err)
			}
			var restored imap.Envelope
			if err := gob.NewDecoder(&encoded).Decode(&restored); err != nil {
				t.Fatal(err)
			}
			if restored.DateUnknownZone || !restored.Date.Equal(legacy.Date) || restored.Subject != legacy.Subject {
				t.Fatalf("legacy payload gained unknown-zone metadata or lost fields: %+v", restored)
			}
			requirePersistedEnvelopeDate(t, &restored, "Tue, 01 Jan 2019 12:00:00 +0000")
		}
	})

	t.Run("nested callback and gob body structure", func(t *testing.T) {
		const knownOuter = "Tue, 01 Jan 2019 12:00:00 +0000"
		const unknownInner = "Wed, 02 Jan 2019 13:00:00 -0000"
		const knownOffset = "Thu, 03 Jan 2019 14:00:00 +0530"
		const knownSibling = "Fri, 04 Jan 2019 15:00:00 +0000"
		message := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
			"--outer\r\nContent-Type: message/rfc822\r\n\r\n" +
			"Date: " + knownOuter + "\r\nSubject: known outer\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
			"--inner\r\nContent-Type: message/global\r\n\r\n" +
			"Date: " + unknownInner + "\r\nSubject: unknown inner\r\nContent-Type: text/plain\r\n\r\nunknown\r\n" +
			"--inner\r\nContent-Type: message/rfc822\r\n\r\n" +
			"Date: " + knownOffset + "\r\nSubject: known offset\r\nContent-Type: text/plain\r\n\r\nknown\r\n" +
			"--inner--\r\n" +
			"--outer\r\nContent-Type: message/global\r\n\r\n" +
			"Date: " + knownSibling + "\r\nSubject: known sibling\r\nContent-Type: text/plain\r\n\r\nsibling\r\n" +
			"--outer--\r\n"
		calls := make(map[string]int)
		options := &ExtractOptions{
			ParseDate: func(string) (time.Time, error) {
				t.Fatal("legacy parser called instead of explicit-zone parser")
				return time.Time{}, nil
			},
			ParseDateWithZone: func(value string) (time.Time, bool, error) {
				calls[value]++
				date, err := mail.ParseDate(value)
				unknown := value == unknownInner
				if unknown {
					// The flag, not the chosen representation of the instant,
					// determines the zone sent by the writer after persistence.
					date = date.In(time.FixedZone("view", 3*3600))
				} else if _, offset := date.Zone(); offset == 0 {
					date = date.In(time.FixedZone("known UTC", 0))
				}
				return date, unknown, err
			},
		}
		body := ExtractBodyStructureWithOptions(strings.NewReader(message), options)
		for _, value := range []string{knownOuter, unknownInner, knownOffset, knownSibling} {
			if calls[value] != 1 {
				t.Errorf("explicit-zone parser calls for %q = %d, want 1", value, calls[value])
			}
		}
		if len(calls) != 4 {
			t.Fatalf("unexpected Date parser inputs: %v", calls)
		}
		var encoded bytes.Buffer
		if err := gob.NewEncoder(&encoded).Encode(&body); err != nil {
			t.Fatal(err)
		}
		var restored imap.BodyStructure
		if err := gob.NewDecoder(&encoded).Decode(&restored); err != nil {
			t.Fatal(err)
		}
		for name, candidate := range map[string]imap.BodyStructure{"extracted": body, "persisted": restored} {
			t.Run(name, func(t *testing.T) {
				var envelopes []*imap.Envelope
				var collect func(imap.BodyStructure)
				collect = func(part imap.BodyStructure) {
					switch part := part.(type) {
					case *imap.BodyStructureMultiPart:
						for _, child := range part.Children {
							collect(child)
						}
					case *imap.BodyStructureSinglePart:
						if part.MessageRFC822 != nil {
							envelopes = append(envelopes, part.MessageRFC822.Envelope)
							collect(part.MessageRFC822.BodyStructure)
						}
					}
				}
				collect(candidate)
				if len(envelopes) != 4 {
					t.Fatalf("got %d attached envelopes, want 4", len(envelopes))
				}
				wire := encodeForTest(func(enc *imapwire.Encoder) { writeBodyStructure(enc, candidate, false) })
				for i, want := range []string{knownOuter, unknownInner, knownOffset, knownSibling} {
					if envelopes[i] == nil || envelopes[i].DateUnknownZone != (i == 1) {
						t.Fatalf("attached envelope %d lost its explicit zone: %+v", i, envelopes[i])
					}
					requirePersistedEnvelopeDate(t, envelopes[i], want)
					if !strings.Contains(wire, "(\""+want+"\" ") {
						t.Errorf("BODYSTRUCTURE omitted or changed attached date %q: %s", want, wire)
					}
				}
			})
		}
	})
}

func requirePersistedEnvelopeDate(t *testing.T, envelope *imap.Envelope, want string) {
	t.Helper()
	got := encodeForTest(func(enc *imapwire.Encoder) { writeEnvelope(enc, envelope) })
	if !strings.HasPrefix(got, "(\""+want+"\" ") {
		t.Errorf("ENVELOPE date = %s, want %q", got, want)
	}
}
