package imapwire

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapnum"
	"github.com/emersion/go-imap/v2/internal/utf7"
)

// This limits the max list nesting depth to prevent stack overflow.
const maxListDepth = 1000

// IsAtomChar returns true if ch is an ATOM-CHAR.
func IsAtomChar(ch byte) bool {
	switch ch {
	case '(', ')', '{', ' ', '%', '*', '"', '\\', ']':
		return false
	default:
		return !unicode.IsControl(rune(ch))
	}
}

// Is non-empty char
func isAStringChar(ch byte) bool {
	return IsAtomChar(ch) || ch == ']'
}

// DecoderExpectError is an error due to the Decoder.Expect family of methods.
type DecoderExpectError struct {
	Message string
}

func (err *DecoderExpectError) Error() string {
	return fmt.Sprintf("imapwire: %v", err.Message)
}

// A Decoder reads IMAP data.
//
// There are multiple families of methods:
//
//   - Methods directly named after IMAP grammar elements attempt to decode
//     said element, and return false if it's another element.
//   - "Expect" methods do the same, but set the decoder error (see Err) on
//     failure.
type Decoder struct {
	// CheckBufferedLiteralFunc is called when a literal is about to be decoded
	// and needs to be fully buffered in memory.
	CheckBufferedLiteralFunc func(size int64, nonSync bool) error
	// MaxSize defines a maximum number of bytes to be read from the input.
	// Literals are ignored.
	MaxSize int64
	// QuotedUTF8 allows raw UTF-8 in quoted strings. This requires IMAP4rev2
	// to be available, or UTF8=ACCEPT to be enabled.
	QuotedUTF8 bool

	r    *bufio.Reader
	side ConnSide
	err  error

	// literal is set while a LiteralReader is open.
	//
	// pendingLiteral holds the still-unread octets of a NON-SYNCHRONIZING
	// literal that was announced but abandoned — one whose size the caller
	// refused, or one left behind by a failing command. Those octets are on the
	// wire already and are part of the command being read (RFC 9051 §2.2.1), so
	// DiscardLine skips them rather than letting the next command start inside
	// them. Synchronizing literals are never recorded: the client is waiting
	// for a continuation request that the failing command never sends, so
	// nothing was transmitted and there is nothing to skip.
	//
	// The count is only trustworthy because abandoning a literal also sets the
	// decoder error, which stops the parser from consuming any more of the
	// payload behind DiscardLine's back.
	literal        bool
	pendingLiteral int64

	// discarding is set while DiscardLine is skipping the rest of a failed
	// command: it is the one reader allowed to run after an error.
	discarding bool

	// desynced records that the command stream could not be resynchronised:
	// octets of the current command remain in the stream and cannot safely be
	// skipped. The connection has to be torn down; see Desynchronized.
	desynced  bool
	crlf      bool
	listDepth int
	readBytes int64
}

// NewDecoder creates a new decoder.
func NewDecoder(r *bufio.Reader, side ConnSide) *Decoder {
	return &Decoder{r: r, side: side}
}

func (dec *Decoder) mustUnreadByte() {
	if err := dec.r.UnreadByte(); err != nil {
		panic(fmt.Errorf("imapwire: failed to unread byte: %v", err))
	}
	dec.readBytes--
}

// Err returns the decoder error, if any.
func (dec *Decoder) Err() error {
	return dec.err
}

// ResetCount resets the running byte counter used by MaxSize.
//
// A long-lived decoder (e.g. the client's, which is reused for every response
// on a connection) must call this at the start of each logical unit so that
// MaxSize acts as a per-unit budget rather than a cumulative cap that would
// eventually trip on a healthy connection. Literal bytes are not counted, so
// this does not affect large streamed payloads.
func (dec *Decoder) ResetCount() {
	dec.readBytes = 0
}

func (dec *Decoder) returnErr(err error) bool {
	if err == nil {
		return true
	}
	if dec.err == nil {
		dec.err = err
	}
	return false
}

func (dec *Decoder) readByte() (byte, bool) {
	// Once the decode has failed, stop consuming input. Parsers routinely try
	// one syntactic form after another, and without this a failed literal would
	// send the next attempt straight into the literal's payload — reading data
	// as syntax, or blocking on octets a client is waiting for a continuation
	// request to send. Only DiscardLine reads past an error, to resynchronise.
	if dec.err != nil && !dec.discarding {
		return 0, false
	}
	if dec.MaxSize > 0 && dec.readBytes > dec.MaxSize {
		return 0, dec.returnErr(fmt.Errorf("imapwire: max size exceeded"))
	}
	dec.crlf = false
	if dec.literal {
		return 0, dec.returnErr(fmt.Errorf("imapwire: cannot decode while a literal is open"))
	}
	b, err := dec.r.ReadByte()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return b, dec.returnErr(err)
	}
	dec.readBytes++
	return b, true
}

func (dec *Decoder) acceptByte(want byte) bool {
	got, ok := dec.readByte()
	if !ok {
		return false
	} else if got != want {
		dec.mustUnreadByte()
		return false
	}
	return true
}

// NextByteIs reports whether the next byte is want, without consuming it.
//
// It is a lookahead for grammars where the production depends on what follows
// rather than on what has already been read. On a read error it returns false
// and sets the decoder error, like any other accept.
func (dec *Decoder) NextByteIs(want byte) bool {
	if !dec.acceptByte(want) {
		return false
	}
	dec.mustUnreadByte()
	return true
}

// EOF returns true if end-of-file is reached.
func (dec *Decoder) EOF() bool {
	_, err := dec.r.ReadByte()
	if err == io.EOF {
		return true
	} else if err != nil {
		return dec.returnErr(err)
	}
	dec.mustUnreadByte()
	return false
}

// Expect sets the decoder error if ok is false.
func (dec *Decoder) Expect(ok bool, name string) bool {
	if !ok {
		msg := fmt.Sprintf("expected %v", name)
		if dec.r.Buffered() > 0 {
			b, _ := dec.r.Peek(1)
			msg += fmt.Sprintf(", got %q", b)
		}
		return dec.returnErr(&DecoderExpectError{Message: msg})
	}
	return true
}

func (dec *Decoder) SP() bool {
	if dec.acceptByte(' ') {
		// https://github.com/emersion/go-imap/issues/571
		b, ok := dec.readByte()
		if !ok {
			return false
		}
		dec.mustUnreadByte()
		return b != '\r' && b != '\n'
	}

	// Special case: SP is optional if the next field is a parenthesized list
	b, ok := dec.readByte()
	if !ok {
		return false
	}
	dec.mustUnreadByte()
	return b == '('
}

func (dec *Decoder) ExpectSP() bool {
	return dec.Expect(dec.SP(), "SP")
}

func (dec *Decoder) CRLF() bool {
	dec.acceptByte(' ')  // https://github.com/emersion/go-imap/issues/540
	dec.acceptByte('\r') // be liberal in what we receive and accept lone LF
	if !dec.acceptByte('\n') {
		return false
	}
	dec.crlf = true
	return true
}

func (dec *Decoder) ExpectCRLF() bool {
	return dec.Expect(dec.CRLF(), "CRLF")
}

func (dec *Decoder) Func(ptr *string, valid func(ch byte) bool) bool {
	var sb strings.Builder
	for {
		b, ok := dec.readByte()
		if !ok {
			return false
		}

		if !valid(b) {
			dec.mustUnreadByte()
			break
		}

		sb.WriteByte(b)
	}
	if sb.Len() == 0 {
		return false
	}
	*ptr = sb.String()
	return true
}

func (dec *Decoder) Atom(ptr *string) bool {
	return dec.Func(ptr, IsAtomChar)
}

func (dec *Decoder) ExpectAtom(ptr *string) bool {
	return dec.Expect(dec.Atom(ptr), "atom")
}

func (dec *Decoder) ExpectNIL() bool {
	var s string
	return dec.ExpectAtom(&s) && dec.Expect(s == "NIL", "NIL")
}

func (dec *Decoder) Special(b byte) bool {
	return dec.acceptByte(b)
}

func (dec *Decoder) ExpectSpecial(b byte) bool {
	return dec.Expect(dec.Special(b), fmt.Sprintf("'%v'", string(b)))
}

func (dec *Decoder) Text(ptr *string) bool {
	var sb strings.Builder
	for {
		b, ok := dec.readByte()
		if !ok {
			return false
		} else if b == '\r' || b == '\n' {
			dec.mustUnreadByte()
			break
		}
		sb.WriteByte(b)
	}
	if sb.Len() == 0 {
		return false
	}
	*ptr = sb.String()
	return true
}

func (dec *Decoder) ExpectText(ptr *string) bool {
	return dec.Expect(dec.Text(ptr), "text")
}

func (dec *Decoder) DiscardUntilByte(untilCh byte) {
	for {
		ch, ok := dec.readByte()
		if !ok {
			return
		} else if ch == untilCh {
			dec.mustUnreadByte()
			return
		}
	}
}

// maxDiscardLiteralSize bounds how many octets of a non-synchronizing literal
// DiscardLine drains while recovering from a command error. It is well above
// the 4096-octet cap LITERAL- puts on non-synchronizing literals; a larger one
// is refused rather than read, so recovery can never be turned into an
// unbounded read.
const maxDiscardLiteralSize = 64 << 10

// DiscardLine skips the remainder of the current line, so that the next
// command can be read from a clean position.
//
// The still-unread octets of a non-synchronizing literal announced by this line
// are skipped too: they are part of the command being read (RFC 9051 §2.2.1),
// and leaving them in the stream would make the next command start inside
// client-supplied data. When they cannot be skipped — too many to read, or a
// literal whose announcement was never parsed — the stream is marked
// desynchronised instead of guessing; see Desynchronized.
func (dec *Decoder) DiscardLine() {
	dec.discarding = true
	defer func() { dec.discarding = false }()

	if n := dec.pendingLiteral; n > 0 {
		dec.pendingLiteral = 0
		if n > maxDiscardLiteralSize {
			dec.desynced = true
			return
		}
		if _, err := io.CopyN(io.Discard, dec.r, n); err != nil {
			dec.returnErr(err)
			dec.desynced = true
			return
		}
		// The rest of the command follows the literal octets.
		dec.crlf = false
	}

	if dec.crlf {
		return
	}

	var text string
	dec.Text(&text)
	dec.CRLF()

	// A line ending in a non-synchronizing literal announcement that the parser
	// never reached: its octets follow, but their count was never validated by
	// this decoder, so skipping them would be a guess. Refuse to resynchronise.
	if dec.side == ConnSideServer && endsWithNonSyncLiteral(text) {
		dec.desynced = true
	}
}

// Desynchronized reports whether the decoder had to give up on resynchronising
// the stream. The remaining input cannot be trusted to start at a command
// boundary, so the connection must be terminated rather than kept in service.
func (dec *Decoder) Desynchronized() bool {
	return dec.desynced
}

// endsWithNonSyncLiteral reports whether text ends with a non-synchronizing
// literal announcement such as "{42+}".
func endsWithNonSyncLiteral(text string) bool {
	if !strings.HasSuffix(text, "+}") {
		return false
	}
	start := strings.LastIndexByte(text, '{')
	if start < 0 {
		return false
	}
	_, err := strconv.ParseInt(text[start+1:len(text)-2], 10, 64)
	return err == nil
}

func (dec *Decoder) DiscardValue() bool {
	var s string
	if dec.String(&s) {
		return true
	}

	isList, err := dec.List(func() error {
		if !dec.DiscardValue() {
			return dec.Err()
		}
		return nil
	})
	if err != nil {
		return false
	} else if isList {
		return true
	}

	if dec.Atom(&s) {
		return true
	}

	dec.Expect(false, "value")
	return false
}

func (dec *Decoder) numberStr() (s string, ok bool) {
	var sb strings.Builder
	for {
		ch, ok := dec.readByte()
		if !ok {
			return "", false
		} else if ch < '0' || ch > '9' {
			dec.mustUnreadByte()
			break
		}
		sb.WriteByte(ch)
	}
	if sb.Len() == 0 {
		return "", false
	}
	return sb.String(), true
}

func (dec *Decoder) Number(ptr *uint32) bool {
	s, ok := dec.numberStr()
	if !ok {
		return false
	}
	v64, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return false // can happen on overflow
	}
	*ptr = uint32(v64)
	return true
}

func (dec *Decoder) ExpectNumber(ptr *uint32) bool {
	return dec.Expect(dec.Number(ptr), "number")
}

func (dec *Decoder) ExpectBodyFldOctets(ptr *uint32) bool {
	// Workaround: some servers incorrectly return "-1" for the body structure
	// size. See:
	// https://github.com/emersion/go-imap/issues/534
	if dec.acceptByte('-') {
		*ptr = 0
		return dec.Expect(dec.acceptByte('1'), "-1 (body-fld-octets workaround)")
	}
	return dec.ExpectNumber(ptr)
}

func (dec *Decoder) Number64(ptr *int64) bool {
	s, ok := dec.numberStr()
	if !ok {
		return false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return false // can happen on overflow
	}
	*ptr = v
	return true
}

func (dec *Decoder) ExpectNumber64(ptr *int64) bool {
	return dec.Expect(dec.Number64(ptr), "number64")
}

func (dec *Decoder) ModSeq(ptr *uint64) bool {
	s, ok := dec.numberStr()
	if !ok {
		return false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return false // can happen on overflow
	}
	*ptr = v
	return true
}

func (dec *Decoder) ExpectModSeq(ptr *uint64) bool {
	return dec.Expect(dec.ModSeq(ptr), "mod-sequence-value")
}

func (dec *Decoder) Quoted(ptr *string) bool {
	return dec.quoted(ptr, false)
}

// maxStrayQuotes bounds the recovery in quoted: the malformation seen in the
// wild is a doubled empty string ("" written as """"), so two is enough, and a
// bound keeps a hostile peer from making us discard an unbounded run.
const maxStrayQuotes = 2

func (dec *Decoder) quoted(ptr *string, allowStrayQuotes bool) bool {
	if !dec.Special('"') {
		return false
	}
	var sb strings.Builder
	for {
		ch, ok := dec.readByte()
		if !ok {
			return false
		}

		if ch == '"' {
			break
		}

		if ch == '\\' {
			ch, ok = dec.readByte()
			if !ok {
				return false
			}
		}

		sb.WriteByte(ch)
	}

	if allowStrayQuotes {
		// A conformant peer always follows the closing quote with a delimiter
		// (SP, '(', ')', ']' or CRLF), so a '"' here means the peer emitted a
		// malformed token and the rest of the response would decode against the
		// wrong offsets. Swallowing the stray run costs nothing on conformant
		// input and keeps one bad field from failing the whole response.
		//
		// Peek rather than readByte: hitting the end of the input here is not
		// an error, and must not set the decoder error.
		for i := 0; i < maxStrayQuotes; i++ {
			b, err := dec.r.Peek(1)
			if err != nil || b[0] != '"' {
				break
			}
			dec.r.Discard(1)
			dec.readBytes++
		}
	}

	*ptr = sb.String()
	return true
}

// ExpectNStringAllowStrayQuotes is ExpectNString, tolerating a malformed quoted
// string that carries stray trailing quotes. Use it only for fields where a
// non-conformant server has been observed in the wild and the value is not
// load-bearing.
func (dec *Decoder) ExpectNStringAllowStrayQuotes(ptr *string) bool {
	var s string
	if dec.Atom(&s) {
		if !dec.Expect(s == "NIL", "nstring") {
			return false
		}
		*ptr = ""
		return true
	}
	if dec.quoted(ptr, true) || dec.Literal(ptr) {
		return true
	}
	return dec.Expect(false, "string")
}

// ExpectStringAllowStrayQuotes is ExpectString with the same tolerance as
// ExpectNStringAllowStrayQuotes.
func (dec *Decoder) ExpectStringAllowStrayQuotes(ptr *string) bool {
	if dec.quoted(ptr, true) || dec.Literal(ptr) {
		return true
	}
	return dec.Expect(false, "string")
}

func (dec *Decoder) ExpectAString(ptr *string) bool {
	if dec.Quoted(ptr) {
		return true
	}
	if dec.Literal(ptr) {
		return true
	}
	// We cannot do dec.Atom(ptr) here because sometimes mailbox names are unquoted,
	// and they can contain special characters like `]`.
	return dec.Expect(dec.Func(ptr, isAStringChar), "ASTRING-CHAR")
}

func (dec *Decoder) String(ptr *string) bool {
	return dec.Quoted(ptr) || dec.Literal(ptr)
}

func (dec *Decoder) ExpectString(ptr *string) bool {
	return dec.Expect(dec.String(ptr), "string")
}

func (dec *Decoder) ExpectNString(ptr *string) bool {
	var s string
	if dec.Atom(&s) {
		if !dec.Expect(s == "NIL", "nstring") {
			return false
		}
		*ptr = ""
		return true
	}
	return dec.ExpectString(ptr)
}

func (dec *Decoder) ExpectNStringReader() (lit *LiteralReader, nonSync, ok bool) {
	var s string
	if dec.Atom(&s) {
		if !dec.Expect(s == "NIL", "nstring") {
			return nil, false, false
		}
		return nil, true, true
	}
	// TODO: read quoted string as a string instead of buffering
	if dec.Quoted(&s) {
		return newLiteralReaderFromString(s), true, true
	}
	if lit, nonSync, ok = dec.LiteralReader(); ok {
		return lit, nonSync, true
	} else {
		return nil, false, dec.Expect(false, "nstring")
	}
}

func (dec *Decoder) List(f func() error) (isList bool, err error) {
	if !dec.Special('(') {
		return false, nil
	}
	if dec.Special(')') {
		return true, nil
	}

	dec.listDepth++
	defer func() {
		dec.listDepth--
	}()

	if dec.listDepth >= maxListDepth {
		return false, fmt.Errorf("imapwire: exceeded max depth")
	}

	for {
		if err := f(); err != nil {
			return true, err
		}

		if dec.Special(')') {
			return true, nil
		} else if !dec.ExpectSP() {
			return true, dec.Err()
		}
	}
}

func (dec *Decoder) ExpectList(f func() error) error {
	isList, err := dec.List(f)
	if err != nil {
		return err
	} else if !dec.Expect(isList, "(") {
		return dec.Err()
	}
	return nil
}

func (dec *Decoder) ExpectNList(f func() error) error {
	var s string
	if dec.Atom(&s) {
		if !dec.Expect(s == "NIL", "NIL") {
			return dec.Err()
		}
		return nil
	}
	return dec.ExpectList(f)
}

func (dec *Decoder) ExpectMailbox(ptr *string) bool {
	var name string
	if !dec.ExpectAString(&name) {
		return false
	}
	if strings.EqualFold(name, "INBOX") {
		*ptr = "INBOX"
		return true
	}

	// The mirror of Encoder.Mailbox: in UTF-8 mode the bytes on the wire are
	// the name. Unescaping "&-" here would fold a mailbox genuinely called
	// "A&-B" -- four ordinary characters to a conformant peer -- down to "A&B".
	if dec.QuotedUTF8 {
		*ptr = name
		return true
	}

	name, err := utf7.Decode(name)
	if err == nil {
		*ptr = name
	}
	return dec.returnErr(err)
}

func (dec *Decoder) ExpectUID(ptr *imap.UID) bool {
	var num uint32
	if !dec.ExpectNumber(&num) {
		return false
	}
	*ptr = imap.UID(num)
	return true
}

func (dec *Decoder) ExpectNumSet(kind NumKind, ptr *imap.NumSet) bool {
	if dec.Special('$') {
		*ptr = imap.SearchRes()
		return true
	}

	var s string
	if !dec.Expect(dec.Func(&s, isNumSetChar), "sequence-set") {
		return false
	}
	numSet, err := imapnum.ParseSet(s)
	if err != nil {
		return dec.returnErr(err)
	}

	switch kind {
	case NumKindSeq:
		*ptr = seqSetFromNumSet(numSet)
	case NumKindUID:
		*ptr = uidSetFromNumSet(numSet)
	}
	return true
}

func (dec *Decoder) ExpectUIDSet(ptr *imap.UIDSet) bool {
	var numSet imap.NumSet
	ok := dec.ExpectNumSet(NumKindUID, &numSet)
	if ok {
		*ptr = numSet.(imap.UIDSet)
	}
	return ok
}

func isNumSetChar(ch byte) bool {
	return ch == '*' || IsAtomChar(ch)
}

func (dec *Decoder) Literal(ptr *string) bool {
	lit, nonSync, ok := dec.LiteralReader()
	if !ok {
		return false
	}
	// Hard upper bound. The IMAP grammar permits arbitrary literal sizes, but no
	// realistic in-band data needs more than this. Callers can opt in to a
	// looser limit by setting CheckBufferedLiteralFunc.
	const absoluteMaxBufferedLiteral = 4 * 1024 * 1024
	if dec.CheckBufferedLiteralFunc != nil {
		if err := dec.CheckBufferedLiteralFunc(lit.Size(), nonSync); err != nil {
			// Fail the whole decode rather than returning a plain false:
			// callers such as ExpectAString would otherwise fall through to
			// another read and start consuming the literal's payload as IMAP
			// syntax — which both corrupts the command and makes the
			// pending-octet count DiscardLine relies on wrong.
			lit.cancel(nil)
			return dec.returnErr(err)
		}
	} else if lit.Size() > absoluteMaxBufferedLiteral {
		lit.cancel(nil)
		return dec.returnErr(fmt.Errorf("imapwire: literal of %d bytes exceeds default buffered cap", lit.Size()))
	}
	var sb strings.Builder
	_, err := io.Copy(&sb, lit)
	if err == nil {
		*ptr = sb.String()
	}
	return dec.returnErr(err)
}

func (dec *Decoder) LiteralReader() (lit *LiteralReader, nonSync, ok bool) {
	if !dec.Special('{') {
		return nil, false, false
	}
	var size int64
	if !dec.ExpectNumber64(&size) {
		return nil, false, false
	}
	// Refuse obviously hostile sizes. The IMAP grammar permits arbitrary
	// 64-bit literal sizes, but no real client or server can deliver more
	// than 1 GiB in a single literal without orchestration outside the
	// protocol.
	const absoluteMaxLiteralSize = 1 << 30
	if size < 0 || size > absoluteMaxLiteralSize {
		dec.returnErr(fmt.Errorf("imapwire: literal size %d out of bounds", size))
		return nil, false, false
	}
	if dec.side == ConnSideServer {
		nonSync = dec.acceptByte('+')
	}
	if !dec.ExpectSpecial('}') || !dec.ExpectCRLF() {
		return nil, false, false
	}
	dec.literal = true
	lit = &LiteralReader{
		dec:     dec,
		size:    size,
		nonSync: nonSync,
		r:       newLimitReader(dec.r, size),
	}
	return lit, nonSync, true
}

func (dec *Decoder) ExpectLiteralReader() (lit *LiteralReader, nonSync bool, err error) {
	lit, nonSync, ok := dec.LiteralReader()
	if !dec.Expect(ok, "literal") {
		return nil, false, dec.Err()
	}
	return lit, nonSync, nil
}

type LiteralReader struct {
	nonSync bool
	dec     *Decoder
	size    int64
	r       io.Reader
}

func newLiteralReaderFromString(s string) *LiteralReader {
	return &LiteralReader{
		size: int64(len(s)),
		r:    strings.NewReader(s),
	}
}

func (lit *LiteralReader) Size() int64 {
	return lit.size
}

func (lit *LiteralReader) Read(b []byte) (int, error) {
	n, err := lit.r.Read(b)
	if err == io.EOF {
		lit.cancel(nil)
	} else if err != nil {
		// Any other failure -- an i/o timeout being the common one -- breaks
		// the literal for good. Release it and record the cause, so that later
		// decodes report the i/o error instead of our own "cannot decode while
		// a literal is open".
		lit.cancel(err)
	}
	return n, err
}

// cancel releases the literal. A non-nil err is recorded as the decoder error,
// first one wins.
func (lit *LiteralReader) cancel(err error) {
	if lit.dec == nil {
		return
	}
	if err != nil {
		lit.dec.returnErr(err)
	}
	lit.dec.literal = false
	// A non-synchronizing literal's octets are already on the wire: remember
	// what is left so DiscardLine can skip it instead of parsing it as the next
	// command. A synchronizing literal was never sent — the continuation
	// request that would have asked for it is not coming — so there is nothing
	// to skip.
	if n := lit.remaining(); n > 0 && lit.nonSync {
		lit.dec.pendingLiteral += n
	}
	lit.dec = nil
}

// remaining reports how many octets of the literal have not been read yet.
func (lit *LiteralReader) remaining() int64 {
	if r, ok := lit.r.(*limitReader); ok {
		return r.left
	}
	return 0
}
