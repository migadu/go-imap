package imapserver

import (
	"bytes"
	"encoding/gob"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// Isolate the application's choice of time.Local from every other test and any
// background connection goroutines. A normal TZ=UTC still has a distinct Local
// location; assigning time.UTC is the configuration that exercises coalescing
// of a decoded fixed-zero location with time.UTC.
func TestKnownEnvelopeDateZoneSurvivesPersistence(t *testing.T) {
	const childEnv = "GOIMAP_TEST_ENVELOPE_PERSISTENCE_LOCAL_UTC"
	if os.Getenv(childEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKnownEnvelopeDateZoneSurvivesPersistence$", "-test.count=1", "-test.timeout=15s")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated UTC persistence checks failed: %v\n%s", err, output)
		}
		return
	}

	originalLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = originalLocal })
	gob.Register(&imap.BodyStructureSinglePart{})

	known := func(location *time.Location) *imap.Envelope {
		return &imap.Envelope{Date: time.Date(2019, 1, 1, 12, 0, 0, 0, location), Subject: "known UTC"}
	}
	cases := []struct {
		name     string
		envelope *imap.Envelope
	}{
		{"fixed zero", known(time.FixedZone("", 0))},
		{"fixed zero named UTC", known(time.FixedZone("UTC", 0))},
	}
	for _, zone := range []string{"UT", "UTC", "GMT", "+0000"} {
		var header textproto.Header
		header.Set("Date", "Tue, 01 Jan 2019 12:00:00 "+zone)
		cases = append(cases, struct {
			name     string
			envelope *imap.Envelope
		}{"extracted " + zone, ExtractEnvelope(header)})
	}

	const wantDate = `"Tue, 01 Jan 2019 12:00:00 +0000"`
	checkEnvelope := func(t *testing.T, envelope *imap.Envelope) {
		t.Helper()
		got := encodeForTest(func(enc *imapwire.Encoder) { writeEnvelope(enc, envelope) })
		if !strings.HasPrefix(got, "("+wantDate+" ") {
			t.Errorf("ENVELOPE changed a known Universal Time zone: %s; want %s", got, wantDate)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkEnvelope(t, tc.envelope)
			t.Run("binary date", func(t *testing.T) {
				encoded, err := tc.envelope.Date.MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				var restored time.Time
				if err := restored.UnmarshalBinary(encoded); err != nil {
					t.Fatal(err)
				}
				checkEnvelope(t, &imap.Envelope{Date: restored})
			})
			t.Run("gob envelope", func(t *testing.T) {
				var encoded bytes.Buffer
				if err := gob.NewEncoder(&encoded).Encode(tc.envelope); err != nil {
					t.Fatal(err)
				}
				var restored imap.Envelope
				if err := gob.NewDecoder(&encoded).Decode(&restored); err != nil {
					t.Fatal(err)
				}
				checkEnvelope(t, &restored)
			})
			t.Run("gob nested body structure", func(t *testing.T) {
				body := &imap.BodyStructureSinglePart{
					Type: "message", Subtype: "rfc822", Encoding: "7bit", Size: 10,
					MessageRFC822: &imap.BodyStructureMessageRFC822{
						Envelope: tc.envelope,
						BodyStructure: &imap.BodyStructureSinglePart{
							Type: "text", Subtype: "plain", Encoding: "7bit", Size: 4,
							Text: &imap.BodyStructureText{NumLines: 1},
						},
						NumLines: 3,
					},
				}
				var encoded bytes.Buffer
				if err := gob.NewEncoder(&encoded).Encode(body); err != nil {
					t.Fatal(err)
				}
				var restored imap.BodyStructureSinglePart
				if err := gob.NewDecoder(&encoded).Decode(&restored); err != nil {
					t.Fatal(err)
				}
				got := encodeForTest(func(enc *imapwire.Encoder) { writeBodyStructure(enc, &restored, false) })
				if !strings.Contains(got, " ("+wantDate+" ") {
					t.Errorf("BODYSTRUCTURE changed a persisted known Universal Time zone: %s; want %s", got, wantDate)
				}
			})
		})
	}
}
