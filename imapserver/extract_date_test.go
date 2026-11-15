package imapserver

import (
	"bufio"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
)

// pdt is how a parser that reads RFC 5322 section 4.3 gives "PDT": -0700.
var pdt = time.Date(1996, time.July, 17, 2, 23, 25, 0, time.FixedZone("", -7*60*60))

// stubParseDate gives pdt for a value ending in "PDT" and an error for
// anything else, and records the values it is called with.
func stubParseDate(calls *[]string) func(string) (time.Time, error) {
	return func(value string) (time.Time, error) {
		*calls = append(*calls, value)
		if strings.HasSuffix(value, "PDT") {
			return pdt, nil
		}
		// A date with an error is no date, whatever else is returned.
		return time.Unix(1, 0), errors.New("unparsed")
	}
}

// TestExtractBodyStructureWithOptionsParseDate: ExtractOptions.ParseDate reads
// the Date field of the envelope of every message/rfc822 and message/global
// part, at any depth.
func TestExtractBodyStructureWithOptionsParseDate(t *testing.T) {
	const msg = "Date: Thu, 18 Jul 1996 00:00:00 +0000\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Date: Wed, 17 Jul 1996 02:23:25 PDT\r\n" +
		"Subject: attached\r\n" +
		"Content-Type: message/global\r\n\r\n" +
		"Date: Wed, 17 Jul 1996 02:23:25 Z\r\n" +
		"Subject: nested\r\n\r\n" +
		"body\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Subject: no date\r\n\r\n" +
		"body\r\n" +
		"--b--\r\n"

	var calls []string
	bs := ExtractBodyStructureWithOptions(strings.NewReader(msg), &ExtractOptions{ParseDate: stubParseDate(&calls)})

	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok || len(mp.Children) != 3 {
		t.Fatalf("ExtractBodyStructureWithOptions = %#v, want a multipart with 3 children", bs)
	}
	attached := mp.Children[1].(*imap.BodyStructureSinglePart).MessageRFC822
	if got := attached.Envelope.Date; !got.Equal(pdt) || zoneOffset(got) != zoneOffset(pdt) {
		t.Errorf("attached message date = %v, want %v", got, pdt)
	}
	nested := attached.BodyStructure.(*imap.BodyStructureSinglePart).MessageRFC822
	if got := nested.Envelope.Date; !got.IsZero() {
		t.Errorf("nested message date = %v, want none for a value ParseDate rejects", got)
	}
	undated := mp.Children[2].(*imap.BodyStructureSinglePart).MessageRFC822
	if got := undated.Envelope.Date; !got.IsZero() {
		t.Errorf("undated message date = %v, want none", got)
	}
	want := []string{"Wed, 17 Jul 1996 02:23:25 PDT", "Wed, 17 Jul 1996 02:23:25 Z"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("ParseDate called with %q, want %q", calls, want)
	}
}

// TestExtractEnvelopeWithOptionsParseDate: ExtractOptions.ParseDate reads the
// envelope date, and without it the date is go-message's.
func TestExtractEnvelopeWithOptionsParseDate(t *testing.T) {
	header, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(
		"Date: Wed, 17 Jul 1996 02:23:25 PDT\r\nSubject: s\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	if got := ExtractEnvelopeWithOptions(header, &ExtractOptions{ParseDate: stubParseDate(&calls)}).Date; !got.Equal(pdt) {
		t.Errorf("date with ParseDate = %v, want %v", got, pdt)
	}

	numeric, err := textproto.ReadHeader(bufio.NewReader(strings.NewReader(
		"Date: Wed, 17 Jul 1996 02:23:25 -0700\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []*ExtractOptions{nil, {}} {
		if got := ExtractEnvelopeWithOptions(numeric, options).Date; !got.Equal(pdt) || zoneOffset(got) != zoneOffset(pdt) {
			t.Errorf("date with options %+v = %v, want %v", options, got, pdt)
		}
	}
	if got := ExtractEnvelope(numeric).Date; !got.Equal(pdt) {
		t.Errorf("ExtractEnvelope date = %v, want %v", got, pdt)
	}
}

func zoneOffset(t time.Time) int {
	_, offset := t.Zone()
	return offset
}
