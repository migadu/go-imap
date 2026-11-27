package imapclient_test

import (
	"context"
	"strings"
	"testing"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/imapserver"
)

type quotaDataSession struct {
	quotaTestSession
	types []imap.QuotaResourceType
	data  []imap.QuotaData
}

func (s quotaDataSession) QuotaResourceTypes() []imap.QuotaResourceType {
	return s.types
}

func (s quotaDataSession) GetQuota(context.Context, string) (*imap.QuotaData, error) {
	return &s.data[0], nil
}

func (s quotaDataSession) GetQuotaRoot(context.Context, string) ([]imap.QuotaData, error) {
	return s.data, nil
}

func TestQuota_resourceData(t *testing.T) {
	tests := []struct {
		name      string
		resources map[imap.QuotaResourceType]imap.QuotaResourceData
		wantList  string // empty: backend data must be refused
	}{
		{name: "nil resources", wantList: "()"},
		{
			name:      "empty resources",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{},
			wantList:  "()",
		},
		{
			name: "lowercase resource key",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				"storage": {Usage: 4, Limit: 17},
			},
			wantList: "(STORAGE 4 17)",
		},
		{
			name: "mixed case resource keys",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				"sToRaGe": {Usage: 4, Limit: 17},
				"message": {Usage: 8, Limit: 30},
			},
			wantList: "(STORAGE 4 17 MESSAGE 8 30)",
		},
		{
			name: "zero limit and exceeded usage",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				imap.QuotaResourceStorage: {Usage: 1, Limit: 0},
			},
			wantList: "(STORAGE 1 0)",
		},
		{
			name: "negative lowercase usage",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				"storage": {Usage: -1, Limit: 17},
			},
		},
		{
			name: "negative mixed case limit",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				"sToRaGe": {Usage: 4, Limit: -1},
			},
		},
		{
			name: "conflicting resource spellings",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				"storage":                 {Usage: 4, Limit: 17},
				imap.QuotaResourceStorage: {Usage: 4, Limit: 30},
			},
		},
		{
			name: "only unadvertised resources",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				imap.QuotaResourceMailbox: {Usage: 1, Limit: 10},
			},
		},
		{
			name: "unadvertised resource filtered",
			resources: map[imap.QuotaResourceType]imap.QuotaResourceData{
				imap.QuotaResourceStorage: {Usage: 4, Limit: 17},
				imap.QuotaResourceMailbox: {Usage: 1, Limit: 10},
			},
			wantList: "(STORAGE 4 17)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRawQuotaConn(t, func(s quotaTestSession) imapserver.Session {
				return quotaDataSession{
					quotaTestSession: s,
					types:            []imap.QuotaResourceType{imap.QuotaResourceStorage, imap.QuotaResourceMessage},
					data:             []imap.QuotaData{{Root: "root", Resources: tc.resources}},
				}
			})
			for _, cmd := range []struct{ tag, command string }{
				{"Q1", "GETQUOTA root"},
				{"Q2", "GETQUOTAROOT INBOX"},
			} {
				got := r.cmd(cmd.tag, cmd.command)
				if tc.wantList == "" {
					if len(got) != 1 || !strings.HasPrefix(got[0], cmd.tag+" NO [SERVERBUG]") {
						t.Errorf("%s = %q, want only NO [SERVERBUG]", cmd.command, got)
					}
					continue
				}
				want := `* QUOTA "root" ` + tc.wantList + "\n" + cmd.tag + " OK GETQUOTA completed"
				if cmd.tag == "Q2" {
					want = "* QUOTAROOT INBOX \"root\"\n* QUOTA \"root\" " + tc.wantList + "\nQ2 OK GETQUOTAROOT completed"
				}
				if strings.Join(got, "\n") != want {
					t.Errorf("%s = %q, want %q", cmd.command, got, want)
				}
			}
			if got := r.cmd("Q3", "NOOP"); len(got) != 1 || !strings.HasPrefix(got[0], "Q3 OK") {
				t.Errorf("NOOP = %q, want OK", got)
			}
		})
	}
}

func TestQuota_validateAllRootsBeforeResponse(t *testing.T) {
	r := newRawQuotaConn(t, func(s quotaTestSession) imapserver.Session {
		return quotaDataSession{
			quotaTestSession: s,
			types:            []imap.QuotaResourceType{imap.QuotaResourceStorage},
			data: []imap.QuotaData{
				{Root: "unlimited"},
				{Root: "invalid", Resources: map[imap.QuotaResourceType]imap.QuotaResourceData{"storage": {Usage: -1, Limit: 17}}},
			},
		}
	})
	if got := r.cmd("Q1", "GETQUOTAROOT INBOX"); len(got) != 1 || !strings.HasPrefix(got[0], "Q1 NO [SERVERBUG]") {
		t.Errorf("GETQUOTAROOT = %q, want only NO [SERVERBUG]", got)
	}
}
