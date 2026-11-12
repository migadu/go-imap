package imapwire

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"
)

func nilCaseReaders() map[string]func(*Decoder) bool {
	return map[string]func(*Decoder) bool{
		"NIL":            (*Decoder).ExpectNIL,
		"nstring":        func(dec *Decoder) bool { var s string; return dec.ExpectNString(&s) && s == "" },
		"loose nstring":  func(dec *Decoder) bool { var s string; return dec.ExpectNStringAllowStrayQuotes(&s) && s == "" },
		"nstring reader": func(dec *Decoder) bool { lit, _, ok := dec.ExpectNStringReader(); return ok && lit == nil },
		"nlist": func(dec *Decoder) bool {
			return dec.ExpectNList(func() error { return fmt.Errorf("NIL invoked list callback") }) == nil
		},
	}
}

func TestNILTokensCaseInsensitive(t *testing.T) {
	for _, side := range []ConnSide{ConnSideClient, ConnSideServer} {
		for name, read := range nilCaseReaders() {
			for _, token := range []string{"NIL", "nil", "Nil", "nIl", "niL", "NIl", "NiL", "nIL"} {
				t.Run(fmt.Sprintf("%v/%s/%s", side, name, token), func(t *testing.T) {
					dec := NewDecoder(bufio.NewReader(strings.NewReader(token+" X\r\n")), side)
					if !read(dec) || dec.Err() != nil {
						t.Fatalf("%s rejected %s: %v", name, token, dec.Err())
					}
					var sentinel string
					if !dec.ExpectSP() || !dec.ExpectAtom(&sentinel) || sentinel != "X" || !dec.ExpectCRLF() {
						t.Fatalf("lost next field: %q, %v", sentinel, dec.Err())
					}
				})
			}
		}
	}
}

func TestNILTokensRejectOtherAtoms(t *testing.T) {
	for name, read := range nilCaseReaders() {
		for _, token := range []string{"NILX", "NULL", "nıl"} {
			t.Run(name+"/"+token, func(t *testing.T) {
				dec := NewDecoder(bufio.NewReader(strings.NewReader(token+"\r\n")), ConnSideClient)
				if read(dec) || dec.Err() == nil {
					t.Fatalf("accepted non-NIL atom %q", token)
				}
			})
		}
	}
}

func TestNILSpellingRemainsStringData(t *testing.T) {
	for _, tc := range []struct{ wire, want string }{{`"nil"`, "nil"}, {"{3}\r\nNiL", "NiL"}} {
		for name, read := range map[string]func(*Decoder, *string) bool{
			"nstring":       (*Decoder).ExpectNString,
			"loose nstring": (*Decoder).ExpectNStringAllowStrayQuotes,
			"nstring reader": func(dec *Decoder, s *string) bool {
				lit, _, ok := dec.ExpectNStringReader()
				if !ok || lit == nil {
					return false
				}
				b, err := io.ReadAll(lit)
				*s = string(b)
				return err == nil
			},
		} {
			t.Run(name+"/"+tc.wire, func(t *testing.T) {
				dec := NewDecoder(bufio.NewReader(strings.NewReader(tc.wire+"\r\n")), ConnSideClient)
				var got string
				if !read(dec, &got) || got != tc.want || !dec.ExpectCRLF() {
					t.Fatalf("string data=(%q,%v), want %q", got, dec.Err(), tc.want)
				}
			})
		}
	}
	for _, name := range []string{"NIL", "nil", "NiL"} {
		dec := NewDecoder(bufio.NewReader(strings.NewReader(name+"\r\n")), ConnSideClient)
		var got string
		if !dec.ExpectMailbox(&got) || got != name || !dec.ExpectCRLF() {
			t.Fatalf("mailbox %q became %q: %v", name, got, dec.Err())
		}
	}
}
