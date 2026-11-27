package imapserver

import (
	"context"
	"fmt"

	"github.com/migadu/go-imap"
	"github.com/migadu/go-imap/internal/imapwire"
)

// SessionQuota is an IMAP session which supports the QUOTA extension
// (RFC 9208).
//
// QUOTA is offered only when imap.CapQuota is also in the connection's
// configured capabilities (Options.Caps, or SessionCapabilities when the
// session implements it), as for NOTIFY and the other optional extensions.
// Otherwise it is not advertised and its commands are refused.
//
// SETQUOTA is not supported: it is always rejected with NOPERM and QUOTASET
// is not advertised.
//
// RFC 9208 ties STATUS items to the advertised resource types: with
// QUOTA=RES-MESSAGE, Session.Status must support StatusOptions.NumDeleted,
// and with QUOTA=RES-STORAGE, StatusOptions.DeletedStorage.
type SessionQuota interface {
	Session

	// QuotaResourceTypes returns the resource types supported by the server.
	// Each is advertised as a QUOTA=RES-<type> capability. QUOTA responses
	// only list these resource types, in this order, with uppercase names.
	//
	// A type must be a non-empty ASCII atom. Invalid types and repeats
	// (compared case-insensitively) are ignored; if no valid type remains,
	// QUOTA is not offered, since RFC 9208 requires at least one
	// QUOTA=RES-<type> capability.
	QuotaResourceTypes() []imap.QuotaResourceType

	// GetQuota returns the usage and limits for a quota root. An unknown
	// root should be reported with an error.
	//
	// A resource without a limit must be left out of QuotaData.Resources. A
	// root with no resource limits has an empty Resources map. Resource names
	// are compared case-insensitively; repeated names in Resources are errors.
	GetQuota(ctx context.Context, root string) (*imap.QuotaData, error)

	// GetQuotaRoot returns the quota roots for a mailbox, along with their
	// usage and limits. The list may be empty if no quota root applies.
	//
	// The mailbox doesn't have to exist (RFC 9208 §4.1.2): a client may ask
	// which quota roots a mailbox would get when it is created.
	// When ACL is supported, the backend must check the rights required by
	// the returned resource types (RFC 9208 §6).
	GetQuotaRoot(ctx context.Context, mailbox string) ([]imap.QuotaData, error)
}

// quotaSession returns the session as a SessionQuota, along with the resource
// types it advertises, or nil if QUOTA is not offered on this connection.
func (c *Conn) quotaSession() (SessionQuota, []imap.QuotaResourceType) {
	session, ok := c.session.(SessionQuota)
	if !ok || !c.configuredCaps().Has(imap.CapQuota) {
		return nil, nil
	}
	types := validQuotaResourceTypes(session.QuotaResourceTypes())
	if len(types) == 0 {
		return nil, nil
	}
	return session, types
}

// validQuotaResourceTypes returns the types which can be written as an atom,
// without repeats, in their original order and with uppercase names.
func validQuotaResourceTypes(types []imap.QuotaResourceType) []imap.QuotaResourceType {
	var l []imap.QuotaResourceType
	seen := make(map[string]struct{}, len(types))
	for _, typ := range types {
		if !isQuotaResourceName(string(typ)) {
			continue
		}
		key := imapwire.ToUpperASCII(string(typ))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		l = append(l, imap.QuotaResourceType(key))
	}
	return l
}

func isQuotaResourceName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || !imapwire.IsAtomChar(s[i]) {
			return false
		}
	}
	return true
}

func (c *Conn) handleGetQuota(dec *imapwire.Decoder) error {
	var root string
	if !dec.ExpectSP() || !dec.ExpectAString(&root) || !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, types := c.quotaSession()
	if session == nil {
		return newClientBugError("QUOTA is not supported")
	}

	data, err := session.GetQuota(c.ctx, root)
	if err != nil {
		return err
	}
	if data == nil {
		return fmt.Errorf("imapserver: GetQuota returned no QuotaData for root %q", root)
	}

	resources, err := quotaResources(types, data)
	if err != nil {
		return err
	}

	return c.writeQuota(data, resources)
}

func (c *Conn) handleGetQuotaRoot(dec *imapwire.Decoder) error {
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	session, types := c.quotaSession()
	if session == nil {
		return newClientBugError("QUOTA is not supported")
	}

	l, err := session.GetQuotaRoot(c.ctx, mailbox)
	if err != nil {
		return err
	}

	resources := make([][]quotaResource, len(l))
	for i := range l {
		resources[i], err = quotaResources(types, &l[i])
		if err != nil {
			return err
		}
	}

	if err := c.writeQuotaRoot(mailbox, l); err != nil {
		return err
	}
	for i := range l {
		if err := c.writeQuota(&l[i], resources[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) handleSetQuota(dec *imapwire.Decoder) error {
	var root string
	if !dec.ExpectSP() || !dec.ExpectAString(&root) || !dec.ExpectSP() {
		return dec.Err()
	}
	err := dec.ExpectList(func() error {
		var (
			name  string
			limit int64
		)
		if !dec.ExpectAtom(&name) || !dec.ExpectSP() || !dec.ExpectNumber64(&limit) {
			return dec.Err()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}

	if err := c.checkState(imap.ConnStateAuthenticated); err != nil {
		return err
	}

	if session, _ := c.quotaSession(); session == nil {
		return newClientBugError("QUOTA is not supported")
	}

	return &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeNoPerm,
		Text: "Quota limits cannot be changed",
	}
}

func (c *Conn) writeQuotaRoot(mailbox string, l []imap.QuotaData) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("QUOTAROOT").SP().Mailbox(mailbox)
	for _, data := range l {
		enc.SP().String(data.Root)
	}
	return enc.CRLF()
}

type quotaResource struct {
	typ  imap.QuotaResourceType
	data imap.QuotaResourceData
}

// quotaResources resolves resource names case-insensitively and returns the
// names and values to write, in the order given by the session.
func quotaResources(types []imap.QuotaResourceType, data *imap.QuotaData) ([]quotaResource, error) {
	supported := make(map[imap.QuotaResourceType]struct{}, len(types))
	for _, typ := range types {
		supported[typ] = struct{}{}
	}
	values := make(map[imap.QuotaResourceType]imap.QuotaResourceData, len(data.Resources))
	for typ, res := range data.Resources {
		key := imap.QuotaResourceType(imapwire.ToUpperASCII(string(typ)))
		if _, ok := supported[key]; !ok {
			continue
		}
		if _, ok := values[key]; ok {
			return nil, fmt.Errorf("imapserver: repeated quota resource %v in root %q", key, data.Root)
		}
		if res.Usage < 0 || res.Limit < 0 {
			return nil, fmt.Errorf("imapserver: negative usage or limit for quota resource %v in root %q", typ, data.Root)
		}
		values[key] = res
	}
	if len(values) == 0 && len(data.Resources) != 0 {
		return nil, fmt.Errorf("imapserver: no advertised resource in quota root %q", data.Root)
	}
	var l []quotaResource
	for _, typ := range types {
		if res, ok := values[typ]; ok {
			l = append(l, quotaResource{typ: typ, data: res})
		}
	}
	return l, nil
}

func (c *Conn) writeQuota(data *imap.QuotaData, resources []quotaResource) error {
	enc := newResponseEncoder(c)
	defer enc.end()
	enc.Atom("*").SP().Atom("QUOTA").SP().String(data.Root).SP()
	enc.List(len(resources), func(i int) {
		res := resources[i]
		enc.Atom(string(res.typ)).SP().Number64(res.data.Usage).SP().Number64(res.data.Limit)
	})
	return enc.CRLF()
}
