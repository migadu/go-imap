package imapserver

import (
	"strings"
	
	"github.com/emersion/go-imap/v2"
)

// isIOSClient returns true if the client identifies as iOS Mail.app
func (c *Conn) isIOSClient() bool {
	c.mutex.Lock()
	clientID := c.clientID
	c.mutex.Unlock()
	
	if clientID == nil {
		return false
	}
	
	// Check if client identifies as iOS Mail
	name := strings.ToLower(clientID.Name)
	vendor := strings.ToLower(clientID.Vendor)
	os := strings.ToLower(clientID.OS)
	
	// Common iOS Mail identification patterns
	isIOSMail := strings.Contains(name, "mail") && 
		(strings.Contains(vendor, "apple") || strings.Contains(os, "ios") || strings.Contains(os, "iphone") || strings.Contains(os, "ipad"))
	
	return isIOSMail
}

// supportsCondStore returns true if the connection supports CONDSTORE extension.
// This checks both session-specific capabilities (if available) and server capabilities,
// as well as enabled capabilities. iOS Mail clients are excluded from CONDSTORE support
// due to compatibility issues.
func (c *Conn) supportsCondStore() bool {
	// Disable CONDSTORE for iOS clients due to compatibility issues
	if c.isIOSClient() {
		return false
	}
	
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		return sessionCaps.Has(imap.CapCondStore) || sessionCaps.Has(imap.CapIMAP4rev2)
	}
	return c.enabled.Has(imap.CapIMAP4rev2) || c.availableCapsSet().Has(imap.CapCondStore) || c.availableCapsSet().Has(imap.CapIMAP4rev2)
}

// supportsQResync returns true if the connection supports QRESYNC extension.
// This checks both session-specific capabilities (if available) and server capabilities.
func (c *Conn) supportsQResync() bool {
	if capSession, ok := c.session.(SessionCapabilities); ok {
		sessionCaps := capSession.GetCapabilities()
		return sessionCaps.Has(imap.CapQResync)
	}
	return c.availableCapsSet().Has(imap.CapQResync)
}
