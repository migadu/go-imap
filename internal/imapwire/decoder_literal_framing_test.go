package imapwire

import "testing"

func TestMalformedLiteralHeaderStopsRecovery(t *testing.T) {
	for _, header := range []string{
		"{3+}x\r\n",
		"{3+x}\r\n",
		"{9223372036854775808+}\r\n",
		"{99999999999x+}\r\n",
		"{-1+}\r\n",
		"{x+}\r\n",
	} {
		t.Run(header, func(t *testing.T) {
			dec := newServerDecoder(header + "abc\r\nnext NOOP\r\n")
			if _, _, ok := dec.LiteralReader(); ok {
				t.Fatal("malformed literal header was accepted")
			}
			expectErrorRecorded(t, dec)
			if !dec.Desynchronized() {
				t.Fatal("malformed header left payload eligible for command recovery")
			}
			remaining := dec.r.Buffered()
			dec.DiscardLine()
			if dec.r.Buffered() != remaining {
				t.Fatal("recovery consumed input after framing became uncertain")
			}
		})
	}
}
