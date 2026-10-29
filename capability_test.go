package imap

import "testing"

// QRESYNC implies CONDSTORE (RFC 7162 §3.2). IMAP4rev2 implies neither: RFC
// 9051 folds neither in (Appendix E) and lists both among the other
// recommended extensions (Appendix F), which a server advertises on its own.
func TestCapSetHasCondStoreQResync(t *testing.T) {
	for _, tc := range []struct {
		name               string
		set                CapSet
		condStore, qresync bool
	}{
		{name: "IMAP4rev2", set: CapSet{CapIMAP4rev2: {}}},
		{name: "IMAP4rev2 CONDSTORE", set: CapSet{CapIMAP4rev2: {}, CapCondStore: {}}, condStore: true},
		{name: "QRESYNC", set: CapSet{CapQResync: {}}, condStore: true, qresync: true},
		{name: "IMAP4rev2 QRESYNC", set: CapSet{CapIMAP4rev2: {}, CapQResync: {}}, condStore: true, qresync: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.set.Has(CapCondStore); got != tc.condStore {
				t.Errorf("Has(CONDSTORE) = %v, want %v", got, tc.condStore)
			}
			if got := tc.set.Has(CapQResync); got != tc.qresync {
				t.Errorf("Has(QRESYNC) = %v, want %v", got, tc.qresync)
			}
		})
	}
}
