package imap

// ThreadAlgorithm is a threading algorithm.
type ThreadAlgorithm string

const (
	ThreadOrderedSubject ThreadAlgorithm = "ORDEREDSUBJECT"
	ThreadReferences     ThreadAlgorithm = "REFERENCES"
)

type ThreadData struct {
	Chain      []uint32
	SubThreads []ThreadData
}
