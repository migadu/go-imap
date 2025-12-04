package imapmemserver

import (
	"bytes"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

func (sess *UserSession) Sort(kind imapserver.NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	return sess.mailbox.Sort(kind, sortCriteria, charset, searchCriteria, options)
}

func (mbox *MailboxView) Sort(kind imapserver.NumKind, sortCriteria []imap.SortCriterion, charset string, searchCriteria *imap.SearchCriteria, options *imap.SortOptions) (*imap.SortData, error) {
	mbox.mutex.Lock()
	defer mbox.mutex.Unlock()

	mbox.staticSearchCriteria(searchCriteria)

	// 1. Search for messages
	var matchingMsgs []*message
	for i, msg := range mbox.l {
		seqNum := mbox.tracker.EncodeSeqNum(uint32(i) + 1)
		if msg.search(seqNum, searchCriteria) {
			matchingMsgs = append(matchingMsgs, msg)
		}
	}

	// 2. Sort the messages
	sorter := &memSort{
		criteria: sortCriteria,
		msgs:     matchingMsgs,
		mbox:     mbox,
	}
	sort.Sort(sorter)

	// 3. Collect the results
	var uidToSeq map[imap.UID]uint32
	if kind != imapserver.NumKindUID {
		uidToSeq = make(map[imap.UID]uint32, len(mbox.l))
		for i, m := range mbox.l {
			// Create a map from UID to current sequence number
			uidToSeq[m.uid] = mbox.tracker.EncodeSeqNum(uint32(i) + 1)
		}
	}

	var data imap.SortData
	for _, msg := range sorter.msgs {
		if kind == imapserver.NumKindUID {
			data.All = append(data.All, uint32(msg.uid))
		} else {
			if seqNum, ok := uidToSeq[msg.uid]; ok && seqNum > 0 {
				data.All = append(data.All, seqNum)
			}
		}
	}

	// 4. Calculate MIN, MAX, COUNT for ESORT
	data.Count = uint32(len(data.All))
	if len(data.All) > 0 {
		// Find the true min and max, regardless of sort order, as required
		// by RFC 5267.
		min, max := data.All[0], data.All[0]
		for _, num := range data.All[1:] {
			if num < min {
				min = num
			}
			if num > max {
				max = num
			}
		}
		data.Min = min
		data.Max = max
	}

	return &data, nil
}

type memSort struct {
	criteria []imap.SortCriterion
	msgs     []*message
	mbox     *MailboxView
}

func (ms *memSort) Len() int {
	return len(ms.msgs)
}

func (ms *memSort) Swap(i, j int) {
	ms.msgs[i], ms.msgs[j] = ms.msgs[j], ms.msgs[i]
}

func (ms *memSort) Less(i, j int) bool {
	msgI, msgJ := ms.msgs[i], ms.msgs[j]

	// Parse headers on-demand for sorting
	var headerI, headerJ gomessage.Header
	var headerIParsed, headerJParsed bool
	var dateI, dateJ time.Time
	var sizeI, sizeJ int

	for _, c := range ms.criteria {
		var cmp int
		switch c.Key {
		case imap.SortKeyArrival:
			cmp = msgI.t.Compare(msgJ.t)
		case imap.SortKeyCc:
			if !headerIParsed {
				headerI, _ = ms.parseHeader(msgI)
				headerIParsed = true
			}
			if !headerJParsed {
				headerJ, _ = ms.parseHeader(msgJ)
				headerJParsed = true
			}
			cmp = strings.Compare(headerI.Get("Cc"), headerJ.Get("Cc"))
		case imap.SortKeyDate:
			if dateI.IsZero() {
				dateI = ms.parseDate(msgI)
			}
			if dateJ.IsZero() {
				dateJ = ms.parseDate(msgJ)
			}
			cmp = dateI.Compare(dateJ)
		case imap.SortKeyDisplayFrom:
			if !headerIParsed {
				headerI, _ = ms.parseHeader(msgI)
				headerIParsed = true
			}
			if !headerJParsed {
				headerJ, _ = ms.parseHeader(msgJ)
				headerJParsed = true
			}
			hI := mail.Header{Header: headerI}
			hJ := mail.Header{Header: headerJ}
			var valI, valJ string
			if addrs, err := hI.AddressList("From"); err == nil && len(addrs) > 0 {
				if addrs[0].Name != "" {
					valI = addrs[0].Name
				} else {
					valI = addrs[0].Address
				}
			}
			if addrs, err := hJ.AddressList("From"); err == nil && len(addrs) > 0 {
				if addrs[0].Name != "" {
					valJ = addrs[0].Name
				} else {
					valJ = addrs[0].Address
				}
			}
			cmp = strings.Compare(valI, valJ)
		case imap.SortKeyDisplayTo:
			if !headerIParsed {
				headerI, _ = ms.parseHeader(msgI)
				headerIParsed = true
			}
			if !headerJParsed {
				headerJ, _ = ms.parseHeader(msgJ)
				headerJParsed = true
			}
			hI := mail.Header{Header: headerI}
			hJ := mail.Header{Header: headerJ}
			var valI, valJ string
			if addrs, err := hI.AddressList("To"); err == nil && len(addrs) > 0 {
				if addrs[0].Name != "" {
					valI = addrs[0].Name
				} else {
					valI = addrs[0].Address
				}
			}
			if addrs, err := hJ.AddressList("To"); err == nil && len(addrs) > 0 {
				if addrs[0].Name != "" {
					valJ = addrs[0].Name
				} else {
					valJ = addrs[0].Address
				}
			}
			cmp = strings.Compare(valI, valJ)
		case imap.SortKeyFrom:
			if !headerIParsed {
				headerI, _ = ms.parseHeader(msgI)
				headerIParsed = true
			}
			if !headerJParsed {
				headerJ, _ = ms.parseHeader(msgJ)
				headerJParsed = true
			}
			hI := mail.Header{Header: headerI}
			hJ := mail.Header{Header: headerJ}
			var valI, valJ string
			if addrs, err := hI.AddressList("From"); err == nil && len(addrs) > 0 {
				valI = addrs[0].Address
			}
			if addrs, err := hJ.AddressList("From"); err == nil && len(addrs) > 0 {
				valJ = addrs[0].Address
			}
			cmp = strings.Compare(valI, valJ)
		case imap.SortKeySize:
			if sizeI == 0 {
				sizeI = len(msgI.buf)
			}
			if sizeJ == 0 {
				sizeJ = len(msgJ.buf)
			}
			if sizeI < sizeJ {
				cmp = -1
			} else if sizeI > sizeJ {
				cmp = 1
			}
		case imap.SortKeySubject:
			if !headerIParsed {
				headerI, _ = ms.parseHeader(msgI)
				headerIParsed = true
			}
			if !headerJParsed {
				headerJ, _ = ms.parseHeader(msgJ)
				headerJParsed = true
			}
			hI := mail.Header{Header: headerI}
			hJ := mail.Header{Header: headerJ}
			subjI, _ := hI.Subject()
			subjJ, _ := hJ.Subject()
			cmp = strings.Compare(subjI, subjJ)
		case imap.SortKeyTo:
			if !headerIParsed {
				headerI, _ = ms.parseHeader(msgI)
				headerIParsed = true
			}
			if !headerJParsed {
				headerJ, _ = ms.parseHeader(msgJ)
				headerJParsed = true
			}
			cmp = strings.Compare(headerI.Get("To"), headerJ.Get("To"))
		}
		if cmp != 0 {
			if c.Reverse {
				return cmp > 0
			}
			return cmp < 0
		}
	}

	return msgI.uid < msgJ.uid // Tie-breaker
}

func (ms *memSort) parseHeader(msg *message) (gomessage.Header, error) {
	mr, err := mail.CreateReader(bytes.NewReader(msg.buf))
	if err != nil {
		return gomessage.Header{}, err
	}
	return mr.Header.Header, nil
}

func (ms *memSort) parseDate(msg *message) time.Time {
	header, err := ms.parseHeader(msg)
	if err != nil {
		return time.Time{}
	}
	h := mail.Header{Header: header}
	date, _ := h.Date()
	return date
}
