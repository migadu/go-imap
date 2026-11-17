package imapmemserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	gomessage "github.com/emersion/go-message"
	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
	"golang.org/x/text/encoding/charmap"
)

func TestTrailingUnicodeSpaceAndNELMultipartDesync(t *testing.T) {
	for _, tc := range []struct {
		name string
		ws   string
	}{
		{"nbsp_u00a0", "\u00a0"},
		{"nel_u0085", "\u0085"},
		{"emsp_u2003", "\u2003"},
		{"ideographic_u3000", "\u3000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Type: multipart/mixed" + tc.ws + "; boundary=x\r\n" +
				"Content-Transfer-Encoding: QUOTED-PR\u0130NTABLE\r\n\r\n" +
				"--x\r\nContent-Type: text/plain\r\n\r\nsecretchild\r\n--x--\r\n"

			bs := imapserver.ExtractBodyStructure(strings.NewReader(raw))
			if mp, ok := bs.(*imap.BodyStructureMultiPart); ok {
				t.Errorf("ExtractBodyStructure classified invalid Content-Type as multipart (Subtype=%q)", mp.Subtype)
			}
			if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}}); !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("DecodeBinarySection(Part=[1]) = (%q, %v), want ErrUnknownCTE", got, err)
			}
			if got, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{}); !errors.Is(err, imapserver.ErrUnknownCTE) {
				t.Errorf("DecodeBinarySection(Part=nil) = (%q, %v), want ErrUnknownCTE", got, err)
			}

			gotPreview, available, err := generatePreview(context.Background(), strings.NewReader(raw))
			if err != nil || !available || gotPreview != "" {
				t.Errorf("generatePreview = (%q, %v, %v), want (\"\", true, nil)", gotPreview, available, err)
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
				t.Errorf("SEARCH BODY matched %v, want []", nums)
			}

			digestRaw := "Content-Type: multipart/digest" + tc.ws + "; boundary=d\r\n\r\n" +
				"--d\r\n\r\nFrom: sender@example.com\r\nSubject: Encapsulated\r\n\r\nDigest child text\r\n--d--\r\n"
			gotDigestPreview, availDigest, err := generatePreview(context.Background(), strings.NewReader(digestRaw))
			if err != nil || !availDigest || gotDigestPreview != "" {
				t.Errorf("digest%q generatePreview = (%q, %v, %v), want (\"\", true, nil)", tc.ws, gotDigestPreview, availDigest, err)
			}
		})
	}
}

func TestCommentOnlyCTEInPreviewAndSearch(t *testing.T) {
	old := gomessage.CharsetReader
	t.Cleanup(func() { gomessage.CharsetReader = old })
	gomessage.CharsetReader = func(charset string, r io.Reader) (io.Reader, error) {
		if charset == "iso-8859-1" {
			return charmap.ISO8859_1.NewDecoder().Reader(r), nil
		}
		return nil, fmt.Errorf("unknown charset %q", charset)
	}

	raw := "Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"Content-Transfer-Encoding: (only a comment)\r\n\r\n" +
		"caf\xe9\r\n"

	sp, ok := imapserver.ExtractBodyStructure(strings.NewReader(raw)).(*imap.BodyStructureSinglePart)
	if !ok || sp.Encoding != "" {
		t.Fatalf("ExtractBodyStructure = %#v, want SinglePart with Encoding=\"\"", sp)
	}
	if bin, err := imapserver.DecodeBinarySection(strings.NewReader(raw), &imap.FetchItemBinarySection{Part: []int{1}}); err != nil || string(bin) != "caf\xe9\r\n" {
		t.Fatalf("DecodeBinarySection = (%q, %v), want (\"caf\\xe9\\r\\n\", nil)", bin, err)
	}

	gotPreview, available, err := generatePreview(context.Background(), strings.NewReader(raw))
	if err != nil || !available || gotPreview != "caf\u00e9" {
		t.Errorf("generatePreview = (%q, %v, %v), want (\"café\", true, nil)", gotPreview, available, err)
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
	view := mbox.NewView()

	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Body: []string{"caf\u00e9"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 1 {
		t.Errorf("SEARCH BODY \"café\" = (%v, %v), want ([1], nil)", res.AllSeqNums(), err)
	}
	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Text: []string{"caf\u00e9"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 1 {
		t.Errorf("SEARCH TEXT \"café\" = (%v, %v), want ([1], nil)", res.AllSeqNums(), err)
	}
	if res, err := view.Search(ctx, imapserver.NumKindSeq, &imap.SearchCriteria{Text: []string{"(only a comment)"}}, &imap.SearchOptions{}); err != nil || len(res.AllSeqNums()) != 1 {
		t.Errorf("SEARCH TEXT \"(only a comment)\" = (%v, %v), want ([1], nil)", res.AllSeqNums(), err)
	}
}
