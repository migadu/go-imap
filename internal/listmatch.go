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

func matchList(name, delim, pattern string) bool {
	// TODO: optimize

	i := strings.IndexAny(pattern, "*%")
	if i == -1 {
		// No more wildcards
		return name == pattern
	}

	// Get parts before and after wildcard
	chunk, wildcard, rest := pattern[0:i], pattern[i], pattern[i+1:]

	// Check that name begins with chunk
	if len(chunk) > 0 && !strings.HasPrefix(name, chunk) {
		return false
	}
	name = strings.TrimPrefix(name, chunk)

	// Expand wildcard
	var j int
	for j = 0; j < len(name); j++ {
		if wildcard == '%' && string(name[j]) == delim {
			break // Stop on delimiter if wildcard is %
		}
		// Try to match the rest from here
		if matchList(name[j:], delim, rest) {
			return true
		}
	}

	return matchList(name[j:], delim, rest)
}
