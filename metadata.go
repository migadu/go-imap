package imap

import "fmt"

// GetMetadataDepth represents the depth parameter for GETMETADATA command.
type GetMetadataDepth int

const (
	GetMetadataDepthZero     GetMetadataDepth = 0
	GetMetadataDepthOne      GetMetadataDepth = 1
	GetMetadataDepthInfinity GetMetadataDepth = -1
)

// String returns the string representation of the depth value.
func (depth GetMetadataDepth) String() string {
	switch depth {
	case GetMetadataDepthZero:
		return "0"
	case GetMetadataDepthOne:
		return "1"
	case GetMetadataDepthInfinity:
		return "infinity"
	default:
		panic(fmt.Errorf("imap: unknown GETMETADATA depth %d", depth))
	}
}

// GetMetadataOptions contains options for the GETMETADATA command.
type GetMetadataOptions struct {
	MaxSize *uint32
	Depth   GetMetadataDepth
}

// GetMetadataData is the data returned by the GETMETADATA command.
type GetMetadataData struct {
	Mailbox string
	Entries map[string]*[]byte
	// LongEntries indicates the size of the largest skipped entry due to MAXSIZE.
	// A value of 0 means no entries were skipped.
	LongEntries      uint32
	ResponseCodeData *MetadataResponseCodeData // Response code data from server (e.g., LONGENTRIES size)
}

// MetadataResponseCodeData contains data for METADATA-specific response codes.
type MetadataResponseCodeData struct {
	// Size is used with LONGENTRIES and MAXSIZE response codes
	Size uint32
}

// ValidateMetadataEntry validates a metadata entry name according to RFC 5464.
// Entry names must:
//   - Start with /private/ or /shared/ (RFC 5464 §3.1: entry names are
//     case-insensitive, so the prefix is matched case-insensitively)
//   - Not contain * or %
//   - Not contain consecutive slashes
//   - Not end with a slash (unless it's just the prefix)
func ValidateMetadataEntry(entry string) error {
	if entry == "" {
		return fmt.Errorf("empty entry name")
	}

	// Must start with /private/ or /shared/. Entry names are case-insensitive
	// (RFC 5464 §3.1), so "/PRIVATE/..." and "/Shared/..." are equally valid;
	// canonicalisation to a single case is the backend's responsibility.
	if !hasPrefixFold(entry, "/private/") && !hasPrefixFold(entry, "/shared/") {
		return fmt.Errorf("entry name must start with /private/ or /shared/")
	}

	// Cannot contain wildcards
	if contains(entry, "*") || contains(entry, "%") {
		return fmt.Errorf("entry name cannot contain wildcards")
	}

	// Cannot have consecutive slashes
	if contains(entry, "//") {
		return fmt.Errorf("entry name cannot contain consecutive slashes")
	}

	// Cannot end with slash (except for the base /private/ or /shared/)
	if entry != "/private/" && entry != "/shared/" && hasSuffix(entry, "/") {
		return fmt.Errorf("entry name cannot end with a slash")
	}

	return nil
}

// Helper functions to avoid importing strings package

// hasPrefixFold reports whether s begins with prefix, comparing ASCII letters
// case-insensitively (RFC 5464 §3.1 makes METADATA entry names case-insensitive).
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if toLowerASCII(s[i]) != toLowerASCII(prefix[i]) {
			return false
		}
	}
	return true
}

func toLowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func contains(s, substr string) bool {
	return indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	n := len(substr)
	if n == 0 {
		return 0
	}
	for i := 0; i <= len(s)-n; i++ {
		if s[i:i+n] == substr {
			return i
		}
	}
	return -1
}
