package imapserver

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// The source-zone status is independent of the time's location. Persisting Date
// and its explicit flag keeps the wire date stable in both ENVELOPE and BODY.
func TestWriteEnvelopeDateZone(t *testing.T) {
	at := func(loc *time.Location) time.Time { return time.Date(2019, 1, 1, 12, 0, 0, 0, loc) }
	cases := []struct {
		name    string
		date    time.Time
		unknown bool
		want    string
	}{
		{"UTC", at(time.UTC), false, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"converted to UTC", at(time.FixedZone("", 3600)).UTC(), false, `"Tue, 01 Jan 2019 11:00:00 +0000"`},
		{"offset zero", at(time.FixedZone("", 0)), false, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"offset zero named UTC", at(time.FixedZone("UTC", 0)), false, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"west", at(time.FixedZone("", -7*3600)), false, `"Tue, 01 Jan 2019 12:00:00 -0700"`},
		{"east", at(time.FixedZone("", 5*3600+45*60)), false, `"Tue, 01 Jan 2019 12:00:00 +0545"`},
		{"unknown UTC", at(time.UTC), true, `"Tue, 01 Jan 2019 12:00:00 -0000"`},
		{"unknown fixed zero", at(time.FixedZone("", 0)), true, `"Tue, 01 Jan 2019 12:00:00 -0000"`},
		{"unknown represented east", at(time.FixedZone("", 5*3600+45*60)), true, `"Tue, 01 Jan 2019 06:15:00 -0000"`},
		{"missing", time.Time{}, false, `NIL`},
		{"missing unknown", time.Time{}, true, `NIL`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, err := tc.date.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			var restored time.Time
			if err := restored.UnmarshalBinary(stored); err != nil {
				t.Fatal(err)
			}
			for _, date := range []struct {
				name string
				date time.Time
			}{{"date", tc.date}, {"restored date", restored}} {
				envelope := &imap.Envelope{Date: date.date, DateUnknownZone: tc.unknown, Subject: "s"}
				if got := encodeForTest(func(enc *imapwire.Encoder) { writeEnvelope(enc, envelope) }); !strings.HasPrefix(got, "("+tc.want+" ") {
					t.Errorf("%s: ENVELOPE %s, want date %s", date.name, got, tc.want)
				}

				part := &imap.BodyStructureSinglePart{
					Type: "message", Subtype: "rfc822", Encoding: "7bit", Size: 10,
					MessageRFC822: &imap.BodyStructureMessageRFC822{
						Envelope: envelope,
						BodyStructure: &imap.BodyStructureSinglePart{
							Type: "text", Subtype: "plain", Encoding: "7bit", Size: 4,
							Text: &imap.BodyStructureText{NumLines: 1},
						},
						NumLines: 3,
					},
				}
				if got := encodeForTest(func(enc *imapwire.Encoder) { writeBodyStructure(enc, part, false) }); !strings.Contains(got, " ("+tc.want+" ") {
					t.Errorf("%s: BODY %s, want an attached envelope date %s", date.name, got, tc.want)
				}
			}
		})
	}
}

// Existing parsers retain their numeric offsets, including known UTC. Only the
// explicit callback opts into unknown-zone output.
func TestExtractEnvelopeDateZone(t *testing.T) {
	header := func(date string) textproto.Header {
		var h textproto.Header
		h.Set("Date", date)
		return h
	}
	utc := &ExtractOptions{ParseDate: func(string) (time.Time, error) {
		return time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC), nil
	}}
	unknown := &ExtractOptions{ParseDateWithZone: func(string) (time.Time, bool, error) {
		return time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC), true, nil
	}}
	for _, tc := range []struct {
		date    string
		options *ExtractOptions
		want    string
	}{
		{"Tue, 01 Jan 2019 12:00:00 UT", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 UTC", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 GMT", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 +0000", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 -0700", nil, `"Tue, 01 Jan 2019 12:00:00 -0700"`},
		{"Tue, 01 Jan 2019 12:00:00 UTC", utc, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 GMT+1", unknown, `"Tue, 01 Jan 2019 12:00:00 -0000"`},
	} {
		envelope := ExtractEnvelopeWithOptions(header(tc.date), tc.options)
		if got := encodeForTest(func(enc *imapwire.Encoder) { writeEnvelope(enc, envelope) }); !strings.HasPrefix(got, "("+tc.want+" ") {
			t.Errorf("Date: %s: ENVELOPE %s, want date %s", tc.date, got, tc.want)
		}
	}
}

func TestExtractEnvelopeDateWithZoneCallback(t *testing.T) {
	date := time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		header      string
		parsed      time.Time
		unknown     bool
		err         error
		wantDate    time.Time
		wantUnknown bool
		wantCalls   int
	}{
		{"known", "known zone", date, false, nil, date, false, 1},
		{"unknown", "unknown zone", date, true, nil, date, true, 1},
		{"error discards value", "invalid zone", date, true, errors.New("invalid"), time.Time{}, false, 1},
		{"zero clears flag", "invalid date", time.Time{}, true, nil, time.Time{}, false, 1},
		{"missing", "", date, true, nil, time.Time{}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h textproto.Header
			if tc.header != "" {
				h.Set("Date", tc.header)
			}
			calls := 0
			options := &ExtractOptions{
				ParseDate: func(string) (time.Time, error) {
					t.Fatal("legacy callback called despite ParseDateWithZone")
					return time.Time{}, nil
				},
				ParseDateWithZone: func(value string) (time.Time, bool, error) {
					calls++
					if value != tc.header {
						t.Errorf("callback received %q, want %q", value, tc.header)
					}
					return tc.parsed, tc.unknown, tc.err
				},
			}
			envelope := ExtractEnvelopeWithOptions(h, options)
			if !envelope.Date.Equal(tc.wantDate) || envelope.DateUnknownZone != tc.wantUnknown || calls != tc.wantCalls {
				t.Errorf("date=%v unknown=%v calls=%d; want date=%v unknown=%v calls=%d", envelope.Date, envelope.DateUnknownZone, calls, tc.wantDate, tc.wantUnknown, tc.wantCalls)
			}
		})
	}
}

func encodeForTest(write func(*imapwire.Encoder)) string {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	write(imapwire.NewEncoder(bw, imapwire.ConnSideServer))
	bw.Flush()
	return buf.String()
}
