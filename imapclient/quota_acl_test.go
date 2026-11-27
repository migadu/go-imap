package imapclient_test

import (
	"reflect"
	"testing"

	"github.com/migadu/go-imap"
)

func TestGetQuotaRootACL(t *testing.T) {
	tests := []struct {
		name       string
		mailbox    string
		rights     imap.RightSet
		limits     map[imap.QuotaResourceType]int64
		wantDenied bool
	}{
		{
			name:    "read allowed",
			mailbox: "INBOX",
			rights:  imap.RightSet("lr"),
			limits:  testQuotaLimits,
		},
		{
			name:       "read denied",
			mailbox:    "INBOX",
			rights:     imap.RightSet("la"),
			limits:     testQuotaLimits,
			wantDenied: true,
		},
		{
			name:       "mixed case inbox read denied",
			mailbox:    "iNbOx",
			rights:     imap.RightSet("la"),
			limits:     testQuotaLimits,
			wantDenied: true,
		},
		{
			name:       "storage only read denied",
			mailbox:    "INBOX",
			rights:     imap.RightSet("la"),
			limits:     map[imap.QuotaResourceType]int64{imap.QuotaResourceStorage: 1024},
			wantDenied: true,
		},
		{
			name:       "message only read denied",
			mailbox:    "INBOX",
			rights:     imap.RightSet("la"),
			limits:     map[imap.QuotaResourceType]int64{imap.QuotaResourceMessage: 100},
			wantDenied: true,
		},
		{
			name:    "no quota root",
			mailbox: "INBOX",
			rights:  imap.RightSet("la"),
		},
		{
			name:    "nonexistent mailbox",
			mailbox: "Nonexistent",
			rights:  imap.RightSet("la"),
			limits:  testQuotaLimits,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newQuotaClientServerPair(t, tc.limits, true)
			if err := client.SetACL("INBOX", testUsername, imap.RightModificationReplace, tc.rights).Wait(); err != nil {
				t.Fatalf("SetACL() = %v", err)
			}
			rights, err := client.MyRights("INBOX").Wait()
			if err != nil {
				t.Fatalf("MyRights() = %v", err)
			}
			if !rights.Rights.Equal(tc.rights) {
				t.Fatalf("MyRights() = %q, want %q", rights.Rights, tc.rights)
			}

			data, err := client.GetQuotaRoot(tc.mailbox).Wait()
			if tc.wantDenied {
				wantIMAPError(t, err, imap.StatusResponseTypeNo, imap.ResponseCodeNoPerm)
			} else {
				if err != nil {
					t.Fatalf("GetQuotaRoot() = %v", err)
				}
				var want []imap.QuotaData
				if len(tc.limits) != 0 {
					want = []imap.QuotaData{testQuotaData}
				}
				if !reflect.DeepEqual(data, want) {
					t.Errorf("GetQuotaRoot() = %v, want %v", data, want)
				}
			}

			// RFC 9208 section 6 imposes no mailbox ACL requirement on
			// GETQUOTA itself. A GETQUOTAROOT refusal must not prevent it.
			if len(tc.limits) != 0 {
				data, err := client.GetQuota("").Wait()
				if err != nil {
					t.Fatalf("GetQuota() = %v", err)
				}
				if len(data.Resources) != len(tc.limits) {
					t.Errorf("GetQuota() resources = %v, want limits %v", data.Resources, tc.limits)
				}
			}
		})
	}
}
