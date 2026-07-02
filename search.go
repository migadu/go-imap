package imap

import (
	"reflect"
	"time"
)

// SearchOptions contains options for the SEARCH command.
type SearchOptions struct {
	// Requires IMAP4rev2 or ESEARCH
	ReturnMin   bool
	ReturnMax   bool
	ReturnAll   bool
	ReturnCount bool
	// Requires IMAP4rev2 or SEARCHRES
	ReturnSave bool
}

// SearchCriteria is a criteria for the SEARCH command.
//
// When multiple fields are populated, the result is the intersection ("and"
// function) of all messages that match the fields.
//
// And, Not and Or can be used to combine multiple criteria together. For
// instance, the following criteria matches messages not containing "hello":
//
//	SearchCriteria{Not: []SearchCriteria{{
//		Body: []string{"hello"},
//	}}}
//
// The following criteria matches messages containing either "hello" or
// "world":
//
//	SearchCriteria{Or: [][2]SearchCriteria{{
//		{Body: []string{"hello"}},
//		{Body: []string{"world"}},
//	}}}
type SearchCriteria struct {
	SeqNum []SeqSet
	UID    []UIDSet

	// Only the date is used, the time and timezone are ignored
	Since      time.Time
	Before     time.Time
	SentSince  time.Time
	SentBefore time.Time

	Header []SearchCriteriaHeaderField
	Body   []string
	Text   []string

	Flag    []Flag
	NotFlag []Flag

	Larger  int64
	Smaller int64

	Not []SearchCriteria
	Or  [][2]SearchCriteria

	ModSeq *SearchCriteriaModSeq // requires CONDSTORE
}

// And intersects two search criteria.
func (criteria *SearchCriteria) And(other *SearchCriteria) {
	criteria.SeqNum = append(criteria.SeqNum, other.SeqNum...)
	criteria.UID = append(criteria.UID, other.UID...)

	criteria.Since = intersectSince(criteria.Since, other.Since)
	criteria.Before = intersectBefore(criteria.Before, other.Before)
	criteria.SentSince = intersectSince(criteria.SentSince, other.SentSince)
	criteria.SentBefore = intersectBefore(criteria.SentBefore, other.SentBefore)

	criteria.Header = append(criteria.Header, other.Header...)
	criteria.Body = append(criteria.Body, other.Body...)
	criteria.Text = append(criteria.Text, other.Text...)

	criteria.Flag = append(criteria.Flag, other.Flag...)
	criteria.NotFlag = append(criteria.NotFlag, other.NotFlag...)

	if criteria.Larger == 0 || other.Larger > criteria.Larger {
		criteria.Larger = other.Larger
	}
	if criteria.Smaller == 0 || other.Smaller < criteria.Smaller {
		criteria.Smaller = other.Smaller
	}

	criteria.Not = append(criteria.Not, other.Not...)
	criteria.Or = append(criteria.Or, other.Or...)
}

func intersectSince(t1, t2 time.Time) time.Time {
	switch {
	case t1.IsZero():
		return t2
	case t2.IsZero():
		return t1
	case t1.After(t2):
		return t1
	default:
		return t2
	}
}

func intersectBefore(t1, t2 time.Time) time.Time {
	switch {
	case t1.IsZero():
		return t2
	case t2.IsZero():
		return t1
	case t1.Before(t2):
		return t1
	default:
		return t2
	}
}

type SearchCriteriaHeaderField struct {
	Key, Value string
}

type SearchCriteriaModSeq struct {
	ModSeq       uint64
	MetadataName string
	MetadataType SearchCriteriaMetadataType
}

type SearchCriteriaMetadataType string

const (
	SearchCriteriaMetadataAll     SearchCriteriaMetadataType = "all"
	SearchCriteriaMetadataPrivate SearchCriteriaMetadataType = "priv"
	SearchCriteriaMetadataShared  SearchCriteriaMetadataType = "shared"
)

// SearchData is the data returned by a SEARCH command.
type SearchData struct {
	All NumSet

	// requires IMAP4rev2 or ESEARCH
	Min   uint32
	Max   uint32
	Count uint32

	// requires MULTISEARCH
	Mailbox string
	// requires MULTISEARCH; emitted alongside Mailbox in the ESEARCH response
	// correlator (RFC 7377 §2.1) so returned UIDs are unambiguous across mailboxes
	UIDValidity uint32

	// requires CONDSTORE
	ModSeq uint64
}

// SearchSource describes the set of mailboxes an ESEARCH command operates on,
// parsed from the "IN (source-options)" clause (RFC 7377 §2.1, filter-mailboxes
// from RFC 5465 §6). Multiple scopes are combined as a union. A zero value (no
// "IN" clause) means the currently selected mailbox.
//
// The IMAP server layer cannot enumerate the user's mailboxes or subscriptions,
// so it hands the parsed SearchSource to the backend, which resolves the scopes.
type SearchSource struct {
	Selected   bool     // the currently selected mailbox
	Inboxes    bool     // INBOX (and, optionally, other new-mail targets)
	Personal   bool     // every mailbox in the user's personal namespace
	Subscribed bool     // every subscribed mailbox
	Subtree    []string // each named mailbox and all its descendants
	SubtreeOne []string // each named mailbox and its immediate children
	Mailboxes  []string // the explicitly named mailbox(es)
}

// IsZero reports whether no source-options were specified, i.e. the command had
// no "IN" clause and must run on the selected mailbox.
func (s *SearchSource) IsZero() bool {
	return s == nil || (!s.Selected && !s.Inboxes && !s.Personal && !s.Subscribed &&
		len(s.Subtree) == 0 && len(s.SubtreeOne) == 0 && len(s.Mailboxes) == 0)
}

// SelectedOnly reports whether the only scope requested is the selected mailbox,
// so the command can take the single-mailbox path (extended SEARCH result format,
// no MAILBOX/UIDVALIDITY correlator).
func (s *SearchSource) SelectedOnly() bool {
	return s != nil && s.Selected && !s.Inboxes && !s.Personal && !s.Subscribed &&
		len(s.Subtree) == 0 && len(s.SubtreeOne) == 0 && len(s.Mailboxes) == 0
}

// AllSeqNums returns All as a slice of sequence numbers.
func (data *SearchData) AllSeqNums() []uint32 {
	seqSet, ok := data.All.(SeqSet)
	if !ok {
		return nil
	}

	// Note: a dynamic sequence set would be a server bug
	nums, ok := seqSet.Nums()
	if !ok {
		panic("imap: SearchData.All is a dynamic number set")
	}
	return nums
}

// AllUIDs returns All as a slice of UIDs.
func (data *SearchData) AllUIDs() []UID {
	uidSet, ok := data.All.(UIDSet)
	if !ok {
		return nil
	}

	// Note: a dynamic sequence set would be a server bug
	uids, ok := uidSet.Nums()
	if !ok {
		panic("imap: SearchData.All is a dynamic number set")
	}
	return uids
}

// searchRes is a special empty UIDSet which can be used as a marker. It has
// a non-zero cap so that its data pointer is non-nil and can be compared.
//
// It's a UIDSet rather than a SeqSet so that it can be passed to the
// UID EXPUNGE command.
var (
	searchRes     = make(UIDSet, 0, 1)
	searchResAddr = reflect.ValueOf(searchRes).Pointer()
)

// SearchRes returns a special marker which can be used instead of a UIDSet to
// reference the last SEARCH result. On the wire, it's encoded as '$'.
//
// It requires IMAP4rev2 or the SEARCHRES extension.
func SearchRes() UIDSet {
	return searchRes
}

// IsSearchRes checks whether a sequence set is a reference to the last SEARCH
// result. See SearchRes.
func IsSearchRes(numSet NumSet) bool {
	return reflect.ValueOf(numSet).Pointer() == searchResAddr
}
