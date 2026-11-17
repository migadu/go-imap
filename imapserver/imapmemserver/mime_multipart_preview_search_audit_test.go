package imapmemserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestTrailingVTFFMultipartSubtypeDesyncAndUnknownCTEBypass(t *testing.T) {
	for _, ws := range []string{"\v", "\f"} {
		raw := "Content-Type: multipart/mixed" + ws + "; boundary=x\r\n" +
			"Content-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\n" +
			"--x\r\nContent-Type: text/plain\r\n\r\nsecretchild\r\n--x--\r\n"

		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw))
		if mp, ok := bs.(*imap.BodyStructureMultiPart); ok {
			t.Errorf("ws=%q: ExtractBodyStructure classified invalid Content-Type as multipart (Subtype=%q)", ws, mp.Subtype)
		}
		if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, imapserver.ErrUnknownCTE) {
			t.Errorf("ws=%q: DecodeBinarySection(Part=[1]) = (%q, %v), want ErrUnknownCTE", ws, got, err)
		}
		if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{}); !errors.Is(err, imapserver.ErrUnknownCTE) {
			t.Errorf("ws=%q: DecodeBinarySection(Part=nil) = (%q, %v), want ErrUnknownCTE", ws, got, err)
		}
	}
}

func TestCommentedOrMalformedMultipartDoesNotBypassUnknownCTE(t *testing.T) {
	for _, ct := range []string{
		"multipart/related (caf\u00e9); boundary=rel",
		"multipart/mixed; boundary=rel; badparam",
	} {
		raw := "Content-Type: " + ct + "\r\n" +
			"Content-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\n" +
			"--rel\r\nContent-Type: text/plain\r\n\r\nsecret\r\n--rel--\r\n"

		if _, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{}); !errors.Is(err, imapserver.ErrUnknownCTE) {
			t.Errorf("ct=%q: DecodeBinarySection(Part=nil) err = %v, want ErrUnknownCTE", ct, err)
		}
		if _, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, imapserver.ErrUnknownCTE) {
			t.Errorf("ct=%q: DecodeBinarySection(Part=[1]) err = %v, want ErrUnknownCTE", ct, err)
		}
	}
}

func TestPreviewUnparseableMultipartSiblingDoesNotAbortFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		part string
	}{
		{
			name: "missing_inner_closing_boundary",
			part: "Content-Type: multipart/related; boundary=rel\r\n\r\n" +
				"--rel\r\nContent-Type: text/html\r\n\r\n<p>Missing inner closing boundary</p>\r\n",
		},
		{
			name: "malformed_inner_part_header",
			part: "Content-Type: multipart/related; boundary=rel\r\n\r\n" +
				"--rel\r\nMalformedHeaderLineWithoutColon\r\n\r\n<p>HTML</p>\r\n--rel--\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Type: multipart/alternative; boundary=alt\r\n\r\n" +
				"--alt\r\n" + tc.part +
				"--alt\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
				"Plain fallback preview\r\n" +
				"--alt--\r\n"

			got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
			if err != nil || !available || got != "Plain fallback preview" {
				t.Errorf("generatePreview = (%q, %v, %v), want (\"Plain fallback preview\", true, nil)", got, available, err)
			}
		})
	}
}

func TestUnknownNonASCIIMultipartSubtypePreviewAndSearchConsistency(t *testing.T) {
	const raw = "Content-Type: multipart/d\u0130gest; boundary=x\r\n\r\n" +
		"--x\r\n\r\nline one\nline two\r\n--x--\r\n"

	gotPreview, available, err := generatePreview(context.Background(), strings.NewReader(raw))
	if err != nil || !available || gotPreview != "line one line two" {
		t.Errorf("generatePreview = (%q, %v, %v), want (\"line one line two\", true, nil)", gotPreview, available, err)
	}

	ctx := context.Background()
	u := NewUser("u", "p")
	if err := u.Create(ctx, "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Append(ctx, "INBOX", strings.NewReader(raw), &imap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	mbox, err := u.mailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mbox.NewView().Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{
		Body: []string{"line one"},
	}, &imap.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if nums := res.AllSeqNums(); len(nums) != 1 || nums[0] != 1 {
		t.Errorf("SEARCH BODY on generic multipart/dİgest = %v, want [1]", nums)
	}
}

func TestSearchMultipartSiblingCodingErrorsAndTopLevelUndecodedCTE(t *testing.T) {
	ctx := context.Background()
	u := NewUser("u", "p")
	if err := u.Create(ctx, "INBOX", nil); err != nil {
		t.Fatal(err)
	}

	const msg1 = "Content-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nX-Note: SiblingOneHeader\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n<p>Caf\xe9</p>\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nUTF8SiblingTarget\r\n" +
		"--alt--\r\n"
	const msg2 = "Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: \vquoted-printable\r\n\r\n" +
		"RawUndecoded=42\r\n"
	const msg3 = "Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64 (comment)\r\n\r\n" +
		"c2VjcmV0\r\n"

	for _, m := range []string{msg1, msg2, msg3} {
		if _, err := u.Append(ctx, "INBOX", strings.NewReader(m), &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mbox, err := u.mailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	view := mbox.NewView()

	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Body: []string{"UTF8SiblingTarget"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 1 {
		t.Errorf("SEARCH BODY after unknown-charset sibling = (%v, %v), want ([1], nil)", res.AllSeqNums(), err)
	}
	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Text: []string{"SiblingOneHeader"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 1 {
		t.Errorf("SEARCH TEXT for header on unknown-charset child = (%v, %v), want ([1], nil)", res.AllSeqNums(), err)
	}
	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Body: []string{"RawUndecoded=42"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 0 {
		t.Errorf("SEARCH BODY on top-level \\vquoted-printable matched undecoded raw body (%v, %v), want ([], nil)", res.AllSeqNums(), err)
	}
	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Body: []string{"c2VjcmV0"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 0 {
		t.Errorf("SEARCH BODY on top-level base64 (comment) matched raw base64 (%v, %v), want ([], nil)", res.AllSeqNums(), err)
	}
	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Body: []string{"secret"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 1 || res.AllSeqNums()[0] != 3 {
		t.Errorf("SEARCH BODY on top-level base64 (comment) decoded match = (%v, %v), want ([3], nil)", res.AllSeqNums(), err)
	}
}
