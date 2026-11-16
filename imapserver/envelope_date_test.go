package imapserver

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// TestWriteEnvelopeDateZone: an envelope date is written in its own zone, except a
// date in UTC (time.UTC), which is written with the zone "-0000". RFC 5322 section
// 3.3: "The form "+0000" SHOULD be used to indicate a time zone at Universal Time.
// Though "-0000" also indicates Universal Time, it is used to indicate that the time
// was generated on a system that may be in a local time zone other than Universal
// Time and that the date-time contains no information about the local time zone."
// A date known to be at Universal Time is given in another location of offset zero,
// such as time.FixedZone("", 0), and keeps "+0000".
//
// The rule holds for the envelope of a message/rfc822 part in BODYSTRUCTURE too, and
// it survives the binary (and so gob) encoding of a time.Time, which keeps the UTC
// location apart from a zone of offset zero: a server that stores body structures
// keeps the zone it extracted.
func TestWriteEnvelopeDateZone(t *testing.T) {
	at := func(loc *time.Location) time.Time { return time.Date(2019, 1, 1, 12, 0, 0, 0, loc) }
	cases := []struct {
		name string
		date time.Time
		want string
	}{
		{"UTC", at(time.UTC), `"Tue, 01 Jan 2019 12:00:00 -0000"`},
		{"converted to UTC", at(time.FixedZone("", 3600)).UTC(), `"Tue, 01 Jan 2019 11:00:00 -0000"`},
		{"offset zero", at(time.FixedZone("", 0)), `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"offset zero named UTC", at(time.FixedZone("UTC", 0)), `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"west", at(time.FixedZone("", -7*3600)), `"Tue, 01 Jan 2019 12:00:00 -0700"`},
		{"east", at(time.FixedZone("", 5*3600+45*60)), `"Tue, 01 Jan 2019 12:00:00 +0545"`},
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
				envelope := &imap.Envelope{Date: date.date, Subject: "s"}
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

// TestExtractEnvelopeDateZone: the default Date parser (net/mail) gives the zones "UT"
// and "UTC" in time.UTC. They name Universal Time (RFC 5322 section 4.3: obs-zone =
// "UT" / "GMT" ; Universal Time), so their envelope date is written "+0000", as every
// zone of offset zero was. A ParseDate option that returns time.UTC gets "-0000".
func TestExtractEnvelopeDateZone(t *testing.T) {
	header := func(date string) textproto.Header {
		var h textproto.Header
		h.Set("Date", date)
		return h
	}
	utc := &ExtractOptions{ParseDate: func(string) (time.Time, error) {
		return time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC), nil
	}}
	for _, tc := range []struct {
		date    string
		options *ExtractOptions
		want    string
	}{
		{"Tue, 01 Jan 2019 12:00:00 UT", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 UTC", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 +0000", nil, `"Tue, 01 Jan 2019 12:00:00 +0000"`},
		{"Tue, 01 Jan 2019 12:00:00 -0700", nil, `"Tue, 01 Jan 2019 12:00:00 -0700"`},
		{"Tue, 01 Jan 2019 12:00:00 GMT+1", utc, `"Tue, 01 Jan 2019 12:00:00 -0000"`},
	} {
		envelope := ExtractEnvelopeWithOptions(header(tc.date), tc.options)
		if got := encodeForTest(func(enc *imapwire.Encoder) { writeEnvelope(enc, envelope) }); !strings.HasPrefix(got, "("+tc.want+" ") {
			t.Errorf("Date: %s: ENVELOPE %s, want date %s", tc.date, got, tc.want)
		}
	}
}

func encodeForTest(write func(*imapwire.Encoder)) string {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	write(imapwire.NewEncoder(bw, imapwire.ConnSideServer))
	bw.Flush()
	return buf.String()
}
