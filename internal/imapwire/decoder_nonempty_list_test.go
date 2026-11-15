package imapwire

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// NonEmptyList reads "(" x *(SP x) ")", where List also takes "()".
func TestDecoderNonEmptyList(t *testing.T) {
	for _, tc := range []struct {
		input  string
		isList bool
		atoms  []string
		empty  bool // a syntax error for the empty list
	}{
		{input: "(A B)", isList: true, atoms: []string{"A", "B"}},
		{input: "(A)", isList: true, atoms: []string{"A"}},
		{input: "()", isList: true, empty: true},
		{input: "A", isList: false},
	} {
		for _, side := range []ConnSide{ConnSideServer, ConnSideClient} {
			dec := NewDecoder(bufio.NewReader(strings.NewReader(tc.input+"\r\n")), side)
			var atoms []string
			isList, err := dec.NonEmptyList(func() error {
				var s string
				if !dec.ExpectAtom(&s) {
					return dec.Err()
				}
				atoms = append(atoms, s)
				return nil
			})
			if isList != tc.isList {
				t.Errorf("%q (side %v): isList = %v, want %v", tc.input, side, isList, tc.isList)
			}
			var expectErr *DecoderExpectError
			if tc.empty {
				if !errors.As(err, &expectErr) || !strings.Contains(expectErr.Message, "list element") {
					t.Errorf("%q (side %v): err = %v, want a DecoderExpectError naming the list element", tc.input, side, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%q (side %v): %v", tc.input, side, err)
			} else if strings.Join(atoms, " ") != strings.Join(tc.atoms, " ") {
				t.Errorf("%q (side %v): read %q, want %q", tc.input, side, atoms, tc.atoms)
			}
		}
	}

	// List keeps taking the empty list.
	dec := NewDecoder(bufio.NewReader(strings.NewReader("()\r\n")), ConnSideServer)
	if isList, err := dec.List(func() error { return errors.New("no element expected") }); !isList || err != nil {
		t.Errorf("List(\"()\") = %v, %v, want true, nil", isList, err)
	}
}

func TestDecoderExpectNonEmptyList(t *testing.T) {
	for _, tc := range []struct {
		input string
		ok    bool
	}{
		{"(A)", true},
		{"()", false},
		{"A", false},
	} {
		dec := NewDecoder(bufio.NewReader(strings.NewReader(tc.input+"\r\n")), ConnSideServer)
		err := dec.ExpectNonEmptyList(func() error {
			var s string
			if !dec.ExpectAtom(&s) {
				return dec.Err()
			}
			return nil
		})
		var expectErr *DecoderExpectError
		if tc.ok && err != nil {
			t.Errorf("%q: %v", tc.input, err)
		} else if !tc.ok && !errors.As(err, &expectErr) {
			t.Errorf("%q: err = %v, want a DecoderExpectError", tc.input, err)
		}
	}
}
