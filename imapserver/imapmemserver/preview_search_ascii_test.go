package imapmemserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	gomessage "github.com/emersion/go-message"
	"golang.org/x/text/encoding/charmap"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestPreviewNonASCIICTECharsetAndRFC822Comments(t *testing.T) {
	t.Run("non_ascii_cte_lookalikes", func(t *testing.T) {
		for _, cte := range []string{
			"QUOTED-PR\u0130NTABLE",
			"7B\u0130T",
			"8B\u0130T",
			"B\u0130NARY",
		} {
			t.Run(cte, func(t *testing.T) {
				single := previewTestEntity("text/plain; charset=utf-8", cte, "", "A=42")
				if _, err := imapserver.DecodeBinarySection(strings.NewReader(single), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, imapserver.ErrUnknownCTE) {
					t.Fatalf("DecodeBinarySection(%q) err = %v, want ErrUnknownCTE", cte, err)
				}

				got, available, err := generatePreview(context.Background(), strings.NewReader(single))
				if err != nil || !available || got != "" {
					t.Errorf("single part with unknown non-ASCII CTE %q: preview = (%q, %v, %v), want (\"\", true, nil)", cte, got, available, err)
				}

				alt := strings.Replace(
					previewTestMultipart("alt",
						single,
						previewTestEntity("text/plain; charset=utf-8", "7bit", "", "fallback plain"),
					),
					"multipart/mixed", "multipart/alternative", 1,
				)
				got, available, err = generatePreview(context.Background(), strings.NewReader(alt))
				if err != nil || !available || got != "fallback plain" {
					t.Errorf("multipart/alternative with unknown non-ASCII CTE %q: preview = (%q, %v, %v), want (\"fallback plain\", true, nil)", cte, got, available, err)
				}
			})
		}
	})

	t.Run("non_ascii_charset_lookalikes", func(t *testing.T) {
		old := gomessage.CharsetReader
		gomessage.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
			if charset == "iso-8859-1" || charset == "gbk" {
				return charmap.ISO8859_1.NewDecoder().Reader(input), nil
			}
			return nil, fmt.Errorf("unknown charset %q", charset)
		}
		t.Cleanup(func() { gomessage.CharsetReader = old })

		for _, cs := range []string{
			"US-ASC\u0130\u0130",
			"\u0130SO-8859-1",
			"GB\u212a",
		} {
			t.Run(cs, func(t *testing.T) {
				single := previewTestEntity(fmt.Sprintf("text/plain; charset=%q", cs), "7bit", "", "lookalike charset text")
				got, available, err := generatePreview(context.Background(), strings.NewReader(single))
				if err != nil || !available || got != "" {
					t.Errorf("single part with unknown non-ASCII charset %q: preview = (%q, %v, %v), want (\"\", true, nil)", cs, got, available, err)
				}

				alt := strings.Replace(
					previewTestMultipart("alt",
						single,
						previewTestEntity("text/plain; charset=utf-8", "7bit", "", "fallback utf8"),
					),
					"multipart/mixed", "multipart/alternative", 1,
				)
				got, available, err = generatePreview(context.Background(), strings.NewReader(alt))
				if err != nil || !available || got != "fallback utf8" {
					t.Errorf("multipart/alternative with unknown non-ASCII charset %q: preview = (%q, %v, %v), want (\"fallback utf8\", true, nil)", cs, got, available, err)
				}
			})
		}
	})

	t.Run("disposition_rfc822_comments", func(t *testing.T) {
		for _, disp := range []string{
			"inline (caf\u00e9); filename=\"note.txt\"",
			"inline (note; extra); filename=\"note.txt\"",
		} {
			raw := previewTestEntity("text/plain; charset=utf-8", "7bit", disp, "visible inline body")
			got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
			if err != nil || !available || got != "visible inline body" {
				t.Errorf("Content-Disposition %q: preview = (%q, %v, %v), want (\"visible inline body\", true, nil)", disp, got, available, err)
			}
		}

		rawLookalike := previewTestEntity("text/plain; charset=utf-8", "7bit", "(; ) \u0130nl\u0130ne", "secret attachment")
		got, available, err := generatePreview(context.Background(), strings.NewReader(rawLookalike))
		if err != nil || !available || got != "" {
			t.Errorf("Content-Disposition \"(; ) İnlİne\": preview = (%q, %v, %v), want (\"\", true, nil)", got, available, err)
		}
	})
}

func TestSearchBodyAndTextNonASCIIMIMELookalikes(t *testing.T) {
	ctx := context.Background()
	u := NewUser("u", "p")
	if err := u.Create(ctx, "INBOX", nil); err != nil {
		t.Fatal(err)
	}

	msg1 := "Content-Type: MULT\u0130PART/MIXED; boundary=x\r\n" +
		"Content-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nsecretchild\r\n--x--\r\n"
	msg2 := "Content-Type: mult\u0131part/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nsecretchild\r\n--x--\r\n"
	msg3 := "Content-Type: text/pla\u0130n\r\n\r\nsubtypeneedle\r\n"
	msg4 := "Content-Type: text/pla\u0131n\r\n\r\nsubtypeneedle\r\n"
	msg5 := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\nA=42\r\n--x--\r\n"
	msg6 := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: x-unknown\r\n\r\nA=42\r\n--x--\r\n"

	for i, raw := range []string{msg1, msg2, msg3, msg4, msg5, msg6} {
		if _, err := u.Append(ctx, "INBOX", strings.NewReader(raw), &imap.AppendOptions{}); err != nil {
			t.Fatalf("Append %d: %v", i+1, err)
		}
	}

	mbox, err := u.mailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	view := mbox.NewView()

	checkNoMatch := func(name string, seqNum uint32, bodyPattern string) {
		t.Helper()
		var seqSet imap.SeqSet
		seqSet.AddNum(seqNum)
		res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{
			SeqNum: []imap.SeqSet{seqSet},
			Body:   []string{bodyPattern},
		}, &imap.SearchOptions{})
		if err != nil {
			t.Fatalf("%s Search error: %v", name, err)
		}
		if nums := res.AllSeqNums(); len(nums) != 0 {
			t.Errorf("%s matched SeqNums %v for BODY %q, want none", name, nums, bodyPattern)
		}
	}

	checkNoMatch("control_dotless_i_multipart(msg2)", 2, "secretchild")
	checkNoMatch("control_dotless_i_subtype(msg4)", 4, "subtypeneedle")
	checkNoMatch("control_ascii_unknown_cte(msg6)", 6, "AB")

	checkNoMatch("non_ascii_MULTIPART_lookalike(msg1)", 1, "secretchild")
	checkNoMatch("non_ascii_text_plaIn_subtype(msg3)", 3, "subtypeneedle")
	checkNoMatch("non_ascii_QUOTED_PRINTABLE_cte(msg5)", 5, "AB")
}
