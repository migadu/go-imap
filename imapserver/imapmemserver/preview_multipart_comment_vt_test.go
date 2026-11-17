package imapmemserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

func TestPreviewMultipartCommentDoesNotAbortSiblingFallback(t *testing.T) {
	const raw = "Content-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\n" +
		"Content-Type: multipart/related (caf\u00e9); boundary=rel\r\n\r\n" +
		"--rel\r\nContent-Type: text/html\r\n\r\n<p>HTML</p>\r\n--rel--\r\n" +
		"--alt\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Plain fallback preview\r\n" +
		"--alt--\r\n"

	got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
	if err != nil || !available || got != "Plain fallback preview" {
		t.Errorf("generatePreview = (%q, %v, %v), want (\"Plain fallback preview\", true, nil)", got, available, err)
	}
}

func TestPreviewAndSearchVerticalTabFormFeedConsistency(t *testing.T) {
	t.Run("vt_multipart_does_not_bypass_unknown_cte_in_preview_or_search", func(t *testing.T) {
		const raw = "Content-Type: \vmultipart/mixed; boundary=x\r\n" +
			"Content-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\n" +
			"--x\r\nContent-Type: text/plain\r\n\r\nsecretchild\r\n--x--\r\n"

		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw))
		if _, ok := bs.(*imap.BodyStructureMultiPart); ok {
			t.Fatalf("ExtractBodyStructure unexpectedly classified VT Content-Type as multipart: %+v", bs)
		}
		if _, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, imapserver.ErrUnknownCTE) {
			t.Fatalf("DecodeBinarySection err = %v, want ErrUnknownCTE", err)
		}

		got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
		if err != nil || !available || got != "" {
			t.Errorf("generatePreview = (%q, %v, %v), want (\"\", true, nil)", got, available, err)
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
			Body: []string{"secretchild"},
		}, &imap.SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if nums := res.AllSeqNums(); len(nums) != 0 {
			t.Errorf("SEARCH BODY matched SeqNums %v for VT multipart lookalike, want none", nums)
		}
	})

	t.Run("vt_disposition_inline_treated_as_unknown_attachment", func(t *testing.T) {
		const raw = "Content-Type: text/plain; charset=utf-8\r\n" +
			"Content-Disposition: \vinline; filename=\"private.txt\"\r\n\r\n" +
			"secret attachment\r\n"

		bs := imapserver.ExtractBodyStructure(strings.NewReader(raw)).(*imap.BodyStructureSinglePart)
		if bs.Extended.Disposition == nil || bs.Extended.Disposition.Value == "inline" {
			t.Fatalf("ExtractBodyStructure unexpectedly normalized VT disposition to inline: %+v", bs.Extended.Disposition)
		}

		got, available, err := generatePreview(context.Background(), strings.NewReader(raw))
		if err != nil || !available || got != "" {
			t.Errorf("generatePreview(\\vinline) = (%q, %v, %v), want (\"\", true, nil)", got, available, err)
		}
	})
}
