package imapserver

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"time"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// ExtractBodySection extracts a section of a message body.
//
// It can be used by server backends to implement Session.Fetch.
func ExtractBodySection(r io.Reader, item *imap.FetchItemBodySection) []byte {
	var (
		header textproto.Header
		body   io.Reader
	)

	br := bufio.NewReader(r)
	// Tolerate a malformed header block (use the partial header and keep reading
	// the body) exactly as ExtractBodyStructure does. Otherwise BODY[n] returns
	// nothing for a message whose BODYSTRUCTURE the same parser happily produces —
	// an internal inconsistency clients see as an advertised part that fetches empty.
	header, _ = textproto.ReadHeader(br)
	body = br

	parentMediaType, header, body := findMessagePart(header, body, item.Part)
	if body == nil {
		return nil
	}

	if len(item.Part) > 0 {
		switch item.Specifier {
		case imap.PartSpecifierHeader, imap.PartSpecifierText:
			header, body = openMessagePart(header, body, parentMediaType)
		}
	}

	// Filter header fields
	if len(item.HeaderFields) > 0 {
		if err := filterHeaderFields(&header, item.HeaderFields, true); err != nil {
			return nil
		}
	}
	if len(item.HeaderFieldsNot) > 0 {
		if err := filterHeaderFields(&header, item.HeaderFieldsNot, false); err != nil {
			return nil
		}
	}

	// Write the requested data to a buffer
	var buf bytes.Buffer

	writeHeader := true
	switch item.Specifier {
	case imap.PartSpecifierNone:
		writeHeader = len(item.Part) == 0
	case imap.PartSpecifierText:
		writeHeader = false
	}
	if writeHeader {
		if err := textproto.WriteHeader(&buf, header); err != nil {
			return nil
		}
	}

	switch item.Specifier {
	case imap.PartSpecifierNone, imap.PartSpecifierText:
		// A truncated or malformed part body makes the multipart reader return
		// io.ErrUnexpectedEOF; keep the bytes already read rather than discarding
		// the whole section, so BODY[n] stays consistent with the size
		// ExtractBodyStructure reports (it derives that size with io.ReadAll,
		// which discards the identical error).
		_, _ = io.Copy(&buf, body)
	}

	return extractPartial(buf.Bytes(), item.Partial)
}

// filterHeaderFields keeps (keep == true) or removes (keep == false) the header
// fields named in names. The field-matching is ASCII-range case insensitive but
// otherwise exact (RFC 9051 section 6.4.5). Header.Del is no substitute: it
// matches by CanonicalMIMEHeaderKey, which leaves a field-name holding a byte
// such as "(" or "/" (valid in RFC 5322, not in an HTTP token) as written.
func filterHeaderFields(header *textproto.Header, names []string, keep bool) error {
	set := make(map[string]struct{}, len(names))
	for _, k := range names {
		set[imapwire.ToLowerASCII(k)] = struct{}{}
	}
	var retained [][]byte
	for field := header.Fields(); field.Next(); {
		if _, ok := set[imapwire.ToLowerASCII(field.Key())]; ok != keep {
			continue
		}
		raw, err := field.Raw()
		if err != nil {
			return err
		}
		retained = append(retained, raw)
	}
	if len(retained) == header.Len() {
		return nil
	}

	// Per-field deletion scans a duplicate key's slice each time. Rebuilding
	// keeps the work linear and preserves the raw bytes. AddRaw prepends, so
	// add retained fields in reverse to preserve their original order.
	var filtered textproto.Header
	for i := len(retained) - 1; i >= 0; i-- {
		filtered.AddRaw(retained[i])
	}
	*header = filtered
	return nil
}

func findMessagePart(header textproto.Header, body io.Reader, partPath []int) (string, textproto.Header, io.Reader) {
	// First part of non-multipart message refers to the message itself
	msgHeader := gomessage.Header{Header: header}
	mediaType, _, _ := msgHeader.ContentType()
	if !strings.HasPrefix(mediaType, "multipart/") && len(partPath) > 0 && partPath[0] == 1 {
		partPath = partPath[1:]
	}

	var parentMediaType string
	for i := 0; i < len(partPath); i++ {
		partNum := partPath[i]

		header, body = openMessagePart(header, body, parentMediaType)

		msgHeader := gomessage.Header{Header: header}
		mediaType, typeParams, _ := msgHeader.ContentType()
		if !strings.HasPrefix(mediaType, "multipart/") {
			if partNum != 1 {
				return "", textproto.Header{}, nil
			}
			continue
		}

		mr := textproto.NewMultipartReader(body, typeParams["boundary"])
		found := false
		for j := 1; j <= partNum; j++ {
			p, err := mr.NextPart()
			if err != nil {
				return "", textproto.Header{}, nil
			}

			if j == partNum {
				parentMediaType = mediaType
				header = p.Header
				body = p
				found = true
				break
			}
		}
		if !found {
			return "", textproto.Header{}, nil
		}
	}

	return parentMediaType, header, body
}

func openMessagePart(header textproto.Header, body io.Reader, parentMediaType string) (textproto.Header, io.Reader) {
	msgHeader := gomessage.Header{Header: header}
	mediaType, _, _ := msgHeader.ContentType()
	if !msgHeader.Has("Content-Type") && parentMediaType == "multipart/digest" {
		mediaType = "message/rfc822"
	}
	if mediaType == "message/rfc822" || mediaType == "message/global" {
		br := bufio.NewReader(body)
		header, _ = textproto.ReadHeader(br)
		return header, br
	}
	return header, body
}

func extractPartial(b []byte, partial *imap.SectionPartial) []byte {
	if partial == nil {
		return b
	}

	if partial.Offset > int64(len(b)) {
		return nil
	}
	// Offset+Size can overflow int64, so clamp the remaining length instead.
	size := int64(len(b)) - partial.Offset
	if partial.Size >= 0 && partial.Size < size {
		size = partial.Size
	}
	return b[partial.Offset : partial.Offset+size]
}

// ErrUnknownCTE is the error DecodeBinarySection and DecodeBinarySectionSize
// return for a section whose Content-Transfer-Encoding they cannot decode. RFC
// 9051 §7.5.2: "If the server does not know how to decode the section's
// Content-Transfer-Encoding, it MUST fail the request and issue a "NO"
// response that contains the "UNKNOWN-CTE" response code."
var ErrUnknownCTE = &imap.Error{
	Type: imap.StatusResponseTypeNo,
	Code: imap.ResponseCodeUnknownCTE,
	Text: "Cannot decode the section's Content-Transfer-Encoding",
}

// DecodeBinarySection returns a section of a message body as FETCH BINARY
// sends it: after decoding its Content-Transfer-Encoding (RFC 9051 §6.4.5),
// in the charset it is stored in, and for a text/* section with CRLF line
// endings (RFC 9051 §7.5.2). It returns ErrUnknownCTE for a section whose
// Content-Transfer-Encoding it does not know.
//
// It can be used by server backends to implement Session.Fetch.
func DecodeBinarySection(r io.Reader, item *imap.FetchItemBinarySection) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeBinarySection(&buf, r, item.Part, false); err != nil {
		return nil, err
	}
	return extractPartial(buf.Bytes(), item.Partial), nil
}

// DecodeBinarySectionSize returns the size of the section DecodeBinarySection
// returns, as FETCH BINARY.SIZE sends it, without holding the section. It
// returns ErrUnknownCTE for a section whose Content-Transfer-Encoding it does
// not know.
//
// It can be used by server backends to implement Session.Fetch.
func DecodeBinarySectionSize(r io.Reader, item *imap.FetchItemBinarySectionSize) (uint32, error) {
	var n countingWriter
	if err := writeBinarySection(&n, r, item.Part, false); err != nil {
		return 0, err
	}
	return uint32(n), nil
}

// ExtractBinarySection is DecodeBinarySection, except that it returns a
// section whose Content-Transfer-Encoding it does not know as stored.
//
// Deprecated: Use DecodeBinarySection. RFC 9051 §7.5.2 requires FETCH BINARY
// to fail with NO [UNKNOWN-CTE] for such a section.
func ExtractBinarySection(r io.Reader, item *imap.FetchItemBinarySection) []byte {
	var buf bytes.Buffer
	_ = writeBinarySection(&buf, r, item.Part, true)
	return extractPartial(buf.Bytes(), item.Partial)
}

// ExtractBinarySectionSize returns the size of the section
// ExtractBinarySection returns.
//
// Deprecated: Use DecodeBinarySectionSize. RFC 9051 §7.5.2 requires FETCH
// BINARY.SIZE to fail with NO [UNKNOWN-CTE] for a section whose
// Content-Transfer-Encoding the server does not know.
func ExtractBinarySectionSize(r io.Reader, item *imap.FetchItemBinarySectionSize) uint32 {
	var n countingWriter
	_ = writeBinarySection(&n, r, item.Part, true)
	return uint32(n)
}

// writeBinarySection writes the section at part of the message in r to w
// after decoding its Content-Transfer-Encoding, and nothing else: BINARY is
// the section "after performing decoding of the section's
// Content-Transfer-Encoding" (RFC 9051 §6.4.5), so the charset stays as
// stored. A text/* section is written with CRLF line endings: "servers MUST
// ensure that textual line-oriented sections are always transmitted using the
// IMAP CRLF line termination syntax, regardless of the underlying storage
// representation of the data on the server" (RFC 9051 §7.5.2). A section whose
// Content-Transfer-Encoding is unknown fails with ErrUnknownCTE, or with
// lenient is written as stored. A section that does not exist writes nothing.
func writeBinarySection(w io.Writer, r io.Reader, part []int, lenient bool) error {
	br := bufio.NewReader(r)
	// Tolerate a malformed header block, matching ExtractBodySection and the
	// lenient ExtractBodyStructure the BODYSTRUCTURE is derived from.
	header, _ := textproto.ReadHeader(br)

	parentMediaType, header, body := findMessagePart(header, br, part)
	if body == nil {
		return nil
	}

	h := gomessage.Header{Header: header}
	mediaType, _, _ := h.ContentType()
	if !h.Has("Content-Type") && parentMediaType == "multipart/digest" {
		mediaType = "message/rfc822" // RFC 2046 §5.1.5
	}
	textual := strings.HasPrefix(mediaType, "text/")

	// gomessage.New decodes the Content-Transfer-Encoding, but also converts a
	// text/* body to UTF-8 when a CharsetReader is installed, so it is given
	// the encoding alone. Like New, ignore the encoding of a multipart, which
	// RFC 2045 §6.4 restricts to the identity encodings.
	if !strings.HasPrefix(mediaType, "multipart/") {
		var cte gomessage.Header
		cte.Set("Content-Transfer-Encoding", cteMechanism(header.Get("Content-Transfer-Encoding")))
		// New returns a usable *Entity even alongside an UnknownEncoding error,
		// with the body left as stored.
		entity, err := gomessage.New(cte, body)
		if gomessage.IsUnknownEncoding(err) {
			if !lenient {
				return ErrUnknownCTE
			}
			textual = false
		}
		body = entity.Body
	}

	if len(part) == 0 {
		if err := textproto.WriteHeader(w, header); err != nil {
			return err
		}
	}
	if textual {
		w = &crlfWriter{w: w}
	}
	// Keep the bytes read; a truncated or malformed part yields io.ErrUnexpectedEOF.
	_, _ = io.Copy(w, body)
	return nil
}

// cteMechanism returns the mechanism a Content-Transfer-Encoding field value
// names, without the comments around it: MIME fields "can include RFC 822
// comments, which have no semantic content and should be ignored during MIME
// processing" (RFC 2045 §1). It is what BODYSTRUCTURE reports as body-fld-enc,
// and what FETCH BINARY decodes: gomessage.New takes the whole value for the
// mechanism, so "base64 (comment)" would otherwise be an unknown encoding.
func cteMechanism(v string) string {
	if !strings.ContainsRune(v, '(') {
		return v
	}
	var b strings.Builder
	depth := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case depth > 0 && c == '\\':
			i++ // quoted-pair
			if i >= len(v) {
				return v
			}
		case c == '(':
			depth++
			b.WriteByte(' ')
		case depth > 0 && c == ')':
			depth--
		case depth == 0:
			b.WriteByte(c)
		}
	}
	if depth != 0 {
		return v
	}
	return strings.TrimSpace(b.String())
}

// crlfWriter writes each LF that does not end a CRLF as CRLF.
type crlfWriter struct {
	w  io.Writer
	cr bool // the last octet written was CR
}

func (cw *crlfWriter) Write(p []byte) (int, error) {
	for done := 0; done < len(p); {
		rest := p[done:]
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			if _, err := cw.w.Write(rest); err != nil {
				return done, err
			}
			cw.cr = rest[len(rest)-1] == '\r'
			break
		}
		cr := cw.cr
		if i > 0 {
			cr = rest[i-1] == '\r'
		}
		eol := "\r\n"
		if cr {
			eol = "\n"
		}
		if _, err := cw.w.Write(rest[:i]); err != nil {
			return done, err
		}
		if _, err := io.WriteString(cw.w, eol); err != nil {
			return done, err
		}
		cw.cr = false
		done += i + 1
	}
	return len(p), nil
}

// countingWriter counts the octets written to it.
type countingWriter int64

func (n *countingWriter) Write(p []byte) (int, error) {
	*n += countingWriter(len(p))
	return len(p), nil
}

// ExtractOptions changes how ExtractEnvelopeWithOptions and
// ExtractBodyStructureWithOptions read a message header. A nil *ExtractOptions
// is the default for every field.
type ExtractOptions struct {
	// ParseDate parses the value of a Date header field into the envelope
	// date. A value it returns an error for gives no date (NIL). If nil,
	// go-message's mail.Header.Date is used, which is net/mail.ParseDate: it
	// reads an obsolete alphabetic zone such as "PDT" in the host's time zone
	// rather than as RFC 5322 section 4.3 defines it.
	ParseDate func(value string) (time.Time, error)
}

func (options *ExtractOptions) date(mh mail.Header) time.Time {
	if options == nil || options.ParseDate == nil {
		date, _ := mh.Date()
		return date
	}
	v := mh.Get("Date")
	if v == "" {
		return time.Time{}
	}
	date, err := options.ParseDate(v)
	if err != nil {
		return time.Time{}
	}
	return date
}

// ExtractEnvelope returns a message envelope from its header.
//
// It can be used by server backends to implement Session.Fetch.
func ExtractEnvelope(h textproto.Header) *imap.Envelope {
	return ExtractEnvelopeWithOptions(h, nil)
}

// ExtractEnvelopeWithOptions is ExtractEnvelope with options.
func ExtractEnvelopeWithOptions(h textproto.Header, options *ExtractOptions) *imap.Envelope {
	mh := mail.Header{Header: gomessage.Header{Header: h}}
	date := options.date(mh)
	subject, _ := mh.Subject()
	inReplyTo, _ := mh.MsgIDList("In-Reply-To")
	messageID, _ := mh.MessageID()
	return &imap.Envelope{
		Date:      date,
		Subject:   subject,
		From:      parseAddressList(mh, "From"),
		Sender:    parseAddressList(mh, "Sender"),
		ReplyTo:   parseAddressList(mh, "Reply-To"),
		To:        parseAddressList(mh, "To"),
		Cc:        parseAddressList(mh, "Cc"),
		Bcc:       parseAddressList(mh, "Bcc"),
		InReplyTo: inReplyTo,
		MessageID: messageID,
	}
}

func parseAddressList(mh mail.Header, k string) []imap.Address {
	// TODO: handle groups
	addrs, _ := mh.AddressList(k)
	var l []imap.Address
	for _, addr := range addrs {
		mailbox, host, ok := strings.Cut(addr.Address, "@")
		if !ok {
			continue
		}
		l = append(l, imap.Address{
			Name:    addr.Name,
			Mailbox: mailbox,
			Host:    host,
		})
	}
	return l
}

// ExtractBodyStructure extracts the structure of a message body.
//
// It can be used by server backends to implement Session.Fetch.
func ExtractBodyStructure(r io.Reader) imap.BodyStructure {
	return ExtractBodyStructureWithOptions(r, nil)
}

// ExtractBodyStructureWithOptions is ExtractBodyStructure with options for the
// envelopes of message/rfc822 and message/global parts.
func ExtractBodyStructureWithOptions(r io.Reader, options *ExtractOptions) imap.BodyStructure {
	br := bufio.NewReader(r)
	header, _ := textproto.ReadHeader(br)
	return extractBodyStructure(header, br, options)
}

func extractBodyStructure(rawHeader textproto.Header, r io.Reader, options *ExtractOptions) imap.BodyStructure {
	header := gomessage.Header{Header: rawHeader}

	mediaType, typeParams, _ := header.ContentType()
	primaryType, subType, _ := strings.Cut(mediaType, "/")

	if primaryType == "multipart" {
		bs := &imap.BodyStructureMultiPart{Subtype: subType}
		mr := textproto.NewMultipartReader(r, typeParams["boundary"])
		for {
			part, _ := mr.NextPart()
			if part == nil {
				break
			}
			bs.Children = append(bs.Children, extractBodyStructure(part.Header, part, options))
		}
		// RFC 2046 §5.1.1 / RFC 3501 body-type-mpart grammar require at least
		// one child part ("1*body").  If the multipart reader yielded nothing
		// (e.g. a malformed message whose boundary never matches), inject a
		// synthetic text/plain child so that writeBodyTypeMpart never receives
		// an empty slice.  We preserve the outer multipart type so that IMAP
		// clients still see the correct Content-Type of the message.
		if len(bs.Children) == 0 {
			body, _ := io.ReadAll(r)
			bs.Children = []imap.BodyStructure{
				&imap.BodyStructureSinglePart{
					Type:    "text",
					Subtype: "plain",
					Params:  map[string]string{"charset": "utf-8"},
					Size:    uint32(len(body)),
					Text: &imap.BodyStructureText{
						NumLines: int64(bytes.Count(body, []byte("\n"))),
					},
					Extended: &imap.BodyStructureSinglePartExt{},
				},
			}
		}
		bs.Extended = &imap.BodyStructureMultiPartExt{
			Params:      typeParams,
			Disposition: getContentDisposition(header),
			Language:    getContentLanguage(header),
			Location:    header.Get("Content-Location"),
		}
		return bs
	} else {
		body, _ := io.ReadAll(r) // TODO: optimize
		bs := &imap.BodyStructureSinglePart{
			Type:        primaryType,
			Subtype:     subType,
			Params:      typeParams,
			ID:          header.Get("Content-Id"),
			Description: header.Get("Content-Description"),
			Encoding:    cteMechanism(header.Get("Content-Transfer-Encoding")),
			Size:        uint32(len(body)),
		}
		if mediaType == "message/rfc822" || mediaType == "message/global" {
			br := bufio.NewReader(bytes.NewReader(body))
			childHeader, _ := textproto.ReadHeader(br)
			bs.MessageRFC822 = &imap.BodyStructureMessageRFC822{
				Envelope:      ExtractEnvelopeWithOptions(childHeader, options),
				BodyStructure: extractBodyStructure(childHeader, br, options),
				NumLines:      int64(bytes.Count(body, []byte("\n"))),
			}
		}
		if primaryType == "text" {
			bs.Text = &imap.BodyStructureText{
				NumLines: int64(bytes.Count(body, []byte("\n"))),
			}
		}
		bs.Extended = &imap.BodyStructureSinglePartExt{
			Disposition: getContentDisposition(header),
			Language:    getContentLanguage(header),
			Location:    header.Get("Content-Location"),
		}
		return bs
	}
}

func getContentDisposition(header gomessage.Header) *imap.BodyStructureDisposition {
	disp, dispParams, _ := header.ContentDisposition()
	if disp == "" {
		return nil
	}
	return &imap.BodyStructureDisposition{
		Value:  disp,
		Params: dispParams,
	}
}

func getContentLanguage(header gomessage.Header) []string {
	v := header.Get("Content-Language")
	if v == "" {
		return nil
	}
	// TODO: handle CFWS
	l := strings.Split(v, ",")
	for i, lang := range l {
		l[i] = strings.TrimSpace(lang)
	}
	return l
}
