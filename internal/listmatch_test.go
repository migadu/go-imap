package internal

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestMatchList(t *testing.T) {
	tests := []struct {
		name, pattern string
		delim         rune
		want          bool
	}{
		{"", "", '/', true},
		{"", "*", '/', true},
		{"", "%", '/', true},
		{"", "a", '/', false},
		{"a", "", '/', false},
		{"INBOX", "INBOX", '/', true},
		{"INBOX", "inbox", '/', false},
		{"a/b/c", "*", '/', true},
		{"a/b/c", "%", '/', false},
		{"a/b/c", "%/%/%", '/', true},
		{"a/b/c", "%/%", '/', false},
		{"a/b/c", "a/%", '/', false},
		{"a/b/c", "a/*", '/', true},
		{"a/b/c", "*/c", '/', true},
		{"a/b/c", "%/c", '/', false},
		{"a/b/c", "a%/b/c", '/', true},
		{"a/b/c", "a%b/c", '/', false},
		{"a/b/c", "a*b/c", '/', true},
		{"a/b/c", "*c", '/', true},
		{"a/b/c", "%c", '/', false},
		{"a/b/c", "***", '/', true},
		{"a/b/c", "%%%", '/', false},
		{"a/b/c", "%*%", '/', true},
		{"a/b/c", "*%*", '/', true},
		{"a/", "%/", '/', true},
		{"a/", "%", '/', false},
		{"/", "%", '/', false},
		{"/", "%/%", '/', true},
		{"//", "%/%", '/', false},
		{"//", "%/%/%", '/', true},
		{"abc", "a%c", '/', true},
		{"abc", "a%b", '/', false},
		{"abc", "%b%", '/', true},
		{"aXbXc", "a*c", '/', true},
		{"aXbXc", "a*b*c*", '/', true},
		{"aXbXc", "a*d*c", '/', false},

		// Without a hierarchy delimiter, "%" is "*".
		{"a/b", "%", 0, true},
		{"a/b", "a%b", 0, true},

		// A multi-byte delimiter is a delimiter too: "%" stops at it, and a
		// byte of it in the name is not a delimiter on its own.
		{"a·b", "*", '·', true},
		{"a·b", "%", '·', false},
		{"a·b", "%·%", '·', true},
		{"a·b", "a·%", '·', true},
		{"a·b", "a%", '·', false},
		{"a\xc2b", "%", '·', true}, // lone first byte of "·"
	}
	for _, test := range tests {
		if got := MatchList(test.name, test.delim, "", test.pattern); got != test.want {
			t.Errorf("MatchList(%q, %q, \"\", %q) = %v, want %v", test.name, test.delim, test.pattern, got, test.want)
		}
	}
}

// matchListReference is the definition of the matcher: the backtracking
// implementation MatchList used to have, kept as the oracle for
// TestMatchListEquivalence. It takes exponential time on adversarial patterns,
// which is why it was replaced; on the short inputs used here it is fast.
func matchListReference(name, delim, pattern string) bool {
	i := strings.IndexAny(pattern, "*%")
	if i == -1 {
		return name == pattern
	}
	chunk, wildcard, rest := pattern[0:i], pattern[i], pattern[i+1:]
	if len(chunk) > 0 && !strings.HasPrefix(name, chunk) {
		return false
	}
	name = strings.TrimPrefix(name, chunk)
	var j int
	for j = 0; j < len(name); j++ {
		if wildcard == '%' && string(name[j]) == delim {
			break
		}
		if matchListReference(name[j:], delim, rest) {
			return true
		}
	}
	return matchListReference(name[j:], delim, rest)
}

// TestMatchListEquivalence checks the matcher against the reference on random
// names and patterns over a small alphabet, where wildcards, literals and
// delimiters collide as often as possible.
func TestMatchListEquivalence(t *testing.T) {
	const nameAlphabet = "ab/"
	const patternAlphabet = "ab/*%"

	rng := rand.New(rand.NewSource(1))
	random := func(alphabet string, maxLen int) string {
		b := make([]byte, rng.Intn(maxLen+1))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}

	for _, delim := range []string{"/", ""} {
		for i := 0; i < 200000; i++ {
			name, pattern := random(nameAlphabet, 8), random(patternAlphabet, 8)
			want := matchListReference(name, delim, pattern)
			if got := matchList(name, delim, pattern); got != want {
				t.Fatalf("matchList(%q, %q, %q) = %v, reference says %v", name, delim, pattern, got, want)
			}
		}
	}
}

// TestMatchListAdversarialPattern verifies that a pattern crafted to make a
// backtracking matcher explode is matched in negligible time. The reference
// implementation needs on the order of 10^15 steps for these inputs.
func TestMatchListAdversarialPattern(t *testing.T) {
	name := strings.Repeat("a", 200)
	tests := []struct {
		pattern string
		want    bool
	}{
		{strings.Repeat("*a", 12) + "*b", false},
		{strings.Repeat("%a", 12) + "%b", false},
		{strings.Repeat("*a", 12) + "*", true},
		{strings.Repeat("*a", 12) + "%b/", false},
		{strings.Repeat("a*", 60) + "b", false},
	}
	for _, test := range tests {
		start := time.Now()
		got := MatchList(name, '/', "", test.pattern)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("MatchList(%d×a, %q) took %v", len(name), test.pattern, elapsed)
		}
		if got != test.want {
			t.Errorf("MatchList(%d×a, %q) = %v, want %v", len(name), test.pattern, got, test.want)
		}
	}
}
