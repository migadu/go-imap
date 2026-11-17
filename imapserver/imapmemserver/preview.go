package imapmemserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
	"github.com/migadu/go-imap/internal/imapwire"
)

// Preview generation is best-effort (RFC 8970 section 8). Limit the entire
// traversal, not just the chosen text: multipart readers also consume headers,
// preambles, and skipped bodies, and nesting makes each byte cross more readers.
const (
	maxPreviewMIMEDepth = 32  // root entity is at depth 1
	maxPreviewMIMEParts = 128 // includes multipart containers and the root
	maxPreviewRawBytes  = 256 << 10
	maxPreviewTextBytes = 64 << 10
)

var errPreviewBudget = errors.New("imapmemserver: preview generation limit reached")

// preview returns up to 200 characters from the first inline text/plain part.
// A resource cutoff is unavailable rather than a permanently empty preview;
// WritePreview maps that to NIL for LAZY and an empty string otherwise.
func (msg *message) preview(ctx context.Context) (string, bool, error) {
	return generatePreview(ctx, bytes.NewReader(msg.buf))
}

func generatePreview(ctx context.Context, r io.Reader) (string, bool, error) {
	raw := &previewInput{ctx: ctx, r: r, remaining: maxPreviewRawBytes}
	g := previewGenerator{ctx: ctx, raw: raw}
	entity, err := readMIMEEntity(raw)
	var (
		text  string
		found bool
	)
	if err == nil || isUnknownBodyCoding(err) {
		text, found, err = g.walk(entity, 1, "", err)
	}
	if found && err == nil {
		return text, true, nil
	}
	// go-message can format reader errors without wrapping them. Retain the
	// original failure beneath the parser, even if it reported ordinary EOF.
	if readErr := g.readError(); readErr != nil {
		err = readErr
	}
	if errors.Is(err, errPreviewBudget) {
		return "", false, nil
	}
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	if raw.err != nil {
		return "", false, raw.err
	}
	if err != nil {
		// Malformed or unsupported MIME has no usable preview. This differs
		// from stopping a valid traversal at a generation limit.
		return "", true, nil
	}
	return text, true, nil
}

type previewGenerator struct {
	ctx   context.Context
	raw   *previewInput
	parts int
}

func (g *previewGenerator) readError() error {
	if err := g.ctx.Err(); err != nil {
		return err
	}
	return g.raw.err
}

func isUnknownBodyCoding(err error) bool {
	return gomessage.IsUnknownCharset(err) || gomessage.IsUnknownEncoding(err)
}

func (g *previewGenerator) walk(entity *gomessage.Entity, depth int, parentMediaType string, partErr error) (string, bool, error) {
	if err := g.readError(); err != nil {
		return "", false, err
	}
	if depth > maxPreviewMIMEDepth || g.parts == maxPreviewMIMEParts {
		return "", false, errPreviewBudget
	}
	g.parts++
	rawDisp := entity.Header.Get("Content-Disposition")
	var disposition string
	// A parser may return a normalized disposition alongside a parameter error,
	// accept vertical tabs/form feeds around the token, or reject RFC 822
	// comments on an otherwise valid token. Check the comment-stripped head
	// token directly before exposing this part's text.
	if rawDisp != "" {
		if dispToken, ok := imapwire.MIMEHeadToken(rawDisp); ok && imapwire.IsASCII(dispToken) {
			disposition = dispToken
		}
	}
	// A multipart disposition applies to its entire subtree (RFC 2183
	// section 2.9). Unknown dispositions are attachments too (section 2.8).
	if rawDisp != "" && disposition != "inline" {
		return "", false, nil
	}
	rawCT := entity.Header.Get("Content-Type")
	mediaType, mediaParams, ctErr := entity.Header.ContentType()
	if rawCT != "" {
		if ctToken, ok := imapwire.MIMEHeadToken(rawCT); !ok || !imapwire.IsMediaTypeToken(ctToken) || (ctErr != nil && mediaType == rawCT) {
			mediaType = ""
		} else {
			mediaType = ctToken
		}
	}
	if parentMediaType == "multipart/digest" && rawCT == "" {
		// go-message defaults every absent type to text/plain. Digest children
		// instead default to message/rfc822 (RFC 2046 section 5.1.5), which this
		// preview algorithm skips. Keep that default local to the direct child.
		mediaType = "message/rfc822"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if mediaParams["boundary"] == "" {
			return "", false, nil
		}
		mr := textproto.NewMultipartReader(entity.Body, mediaParams["boundary"])
		for {
			if err := g.readError(); err != nil {
				return "", false, err
			}
			rawPart, nextErr := mr.NextPart()
			var part *gomessage.Entity
			if nextErr == nil {
				part, nextErr = newMIMEEntity(gomessage.Header{Header: rawPart.Header}, rawPart)
			}
			if readErr := g.readError(); readErr != nil {
				return "", false, readErr
			}
			if nextErr == io.EOF {
				return "", false, nil
			}
			if nextErr != nil && !isUnknownBodyCoding(nextErr) {
				return "", false, nil
			}
			text, found, err := g.walk(part, depth+1, mediaType, nextErr)
			if found || err != nil {
				return text, found, err
			}
		}
	}
	if mediaType != "text/plain" || partErr != nil {
		return "", false, nil
	}
	mech := imapwire.CTEMechanism(entity.Header.Get("Content-Transfer-Encoding"))
	if !imapwire.IsASCII(mech) || strings.ContainsAny(mech, "()") {
		return "", false, nil
	}
	if cs, ok := mediaParams["charset"]; ok && !imapwire.IsASCII(cs) {
		return "", false, nil
	}
	// Body is already transfer/charset decoded by go-message. Keep a useful
	// prefix without draining the rest, but check cancellation through buffers.
	body := previewContextReader{ctx: g.ctx, r: entity.Body}
	b, _ := io.ReadAll(io.LimitReader(body, maxPreviewTextBytes))
	text := formatPreviewText(b)
	if err := g.readError(); err != nil {
		if errors.Is(err, errPreviewBudget) && g.ctx.Err() == nil && text != "" {
			return text, true, nil
		}
		return "", false, err
	}
	return text, true, nil
}

func formatPreviewText(b []byte) string {
	text := strings.Join(strings.Fields(strings.ToValidUTF8(string(b), "")), " ")
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	return text
}

// previewInput sits below every multipart reader, so implicit discards and
// nested headers share one limit. Exhaustion is an error, not synthetic EOF.
type previewInput struct {
	ctx       context.Context
	r         io.Reader
	remaining int64
	err       error
}

func (r *previewInput) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	if r.remaining == 0 {
		r.err = errPreviewBudget
		return 0, r.err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

type previewContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r previewContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
