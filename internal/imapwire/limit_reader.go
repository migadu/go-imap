package imapwire

import "io"

// limitReader reads at most n bytes from an underlying reader.
//
// It behaves like io.LimitReader except for a short read: reaching the
// underlying io.EOF before n bytes have been delivered yields
// io.ErrUnexpectedEOF. A literal announces its exact size up front, so a peer
// that stops early has truncated it, and reporting that as a clean io.EOF would
// hand the caller a short value indistinguishable from a complete one.
type limitReader struct {
	r    io.Reader
	left int64
}

func newLimitReader(r io.Reader, n int64) *limitReader {
	return &limitReader{r: r, left: n}
}

func (lr *limitReader) Read(p []byte) (int, error) {
	if lr.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > lr.left {
		p = p[:lr.left]
	}
	n, err := lr.r.Read(p)
	lr.left -= int64(n)
	if err == io.EOF && lr.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
