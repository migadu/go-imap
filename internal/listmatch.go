package internal

import "strings"

// MatchList checks whether a reference and a pattern matches a mailbox.
//
// It is used by the server to answer LIST commands, and by the client to tell
// the LIST responses of its own command apart from unsolicited ones (which
// NOTIFY delivers for MailboxName and SubscriptionChange events).
func MatchList(name string, delim rune, reference, pattern string) bool {
	var delimStr string
	if delim != 0 {
		delimStr = string(delim)
	}

	if delimStr != "" && strings.HasPrefix(pattern, delimStr) {
		reference = ""
		pattern = strings.TrimPrefix(pattern, delimStr)
	}
	if reference != "" {
		if delimStr != "" && !strings.HasSuffix(reference, delimStr) {
			reference += delimStr
		}
		if !strings.HasPrefix(name, reference) {
			return false
		}
		name = strings.TrimPrefix(name, reference)
	}

	return matchList(name, delimStr, pattern)
}

// matchList reports whether pattern matches name (RFC 9051 section 6.3.9): "*"
// matches any sequence of bytes, "%" any sequence of bytes not containing the
// hierarchy delimiter, and everything else matches itself. With an empty delim,
// "%" is the same as "*".
//
// The pattern comes from the client, so the running time must not depend on
// how it is written. A backtracking matcher takes exponential time on
// "*a*a*a*a*b" against a name of a's — enough for one LIST to hold a CPU core
// (and, in a backend that matches under a lock, every other session of the
// user) for hours. This one is a dynamic programme over the name that runs in
// O(len(name)²) time and O(len(name)) space whatever the pattern: a run of
// wildcards collapses to one, and a pattern with more literal bytes than the
// name cannot match, which bounds the number of pattern steps by the name.
func matchList(name, delim, pattern string) bool {
	literals := 0
	for i := 0; i < len(pattern); i++ {
		if c := pattern[i]; c != '*' && c != '%' {
			literals++
		}
	}
	if literals > len(name) {
		return false
	}
	if literals == len(pattern) {
		return name == pattern
	}

	// reach[j] reports whether the part of the pattern consumed so far matches
	// name[:j]; next receives the same for the part including the current step.
	n := len(name)
	buf := make([]bool, 2*(n+1))
	reach, next := buf[:n+1], buf[n+1:]
	reach[0] = true

	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '*' && c != '%' {
			// A literal byte consumes exactly one byte of the name.
			next[0] = false
			for j := 1; j <= n; j++ {
				next[j] = reach[j-1] && name[j-1] == c
			}
		} else {
			// A run of wildcards is one wildcard: any "*" in it lets the run
			// match anything, "%" alone still cannot span a delimiter.
			star := c == '*'
			for i+1 < len(pattern) && (pattern[i+1] == '*' || pattern[i+1] == '%') {
				i++
				star = star || pattern[i] == '*'
			}

			// The wildcard extends every reachable position to the right:
			// next[j] is set if reach[k] is, for some k <= j whose gap
			// name[k:j] the wildcard may absorb. acc carries that OR along j;
			// for "%" it starts over each time a delimiter completes at j, since
			// no earlier k can then reach past it.
			acc := false
			for j := 0; j <= n; j++ {
				if !star && j >= len(delim) && len(delim) > 0 && name[j-len(delim):j] == delim {
					acc = false
					for k := j - len(delim) + 1; k < j; k++ {
						acc = acc || reach[k]
					}
				}
				acc = acc || reach[j]
				next[j] = acc
			}
		}

		reach, next = next, reach
		if !anyTrue(reach) {
			return false
		}
	}
	return reach[n]
}

func anyTrue(s []bool) bool {
	for _, b := range s {
		if b {
			return true
		}
	}
	return false
}
