// Package imapmemserver implements an in-memory IMAP server.
package imapmemserver

import (
	"sync"

	"github.com/emersion/go-imap/v2/imapserver"
)

// Server is a server instance.
//
// A server contains a list of users.
type Server struct {
	mutex sync.Mutex
	users map[string]*User
}

// New creates a new server.
func New() *Server {
	return &Server{
		users: make(map[string]*User),
	}
}

// NewSession creates a new IMAP session.
func (s *Server) NewSession() imapserver.Session {
	return &serverSession{server: s}
}

func (s *Server) user(username string) *User {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.users[username]
}

// AddUser adds a user to the server.
func (s *Server) AddUser(user *User) {
	s.mutex.Lock()
	s.users[user.username] = user
	s.mutex.Unlock()
}

type serverSession struct {
	*UserSession // may be nil

	server *Server // immutable
}

var _ imapserver.Session = (*serverSession)(nil)
var _ imapserver.SessionAppendLimit = (*serverSession)(nil)

// AppendLimit implements the SessionAppendLimit interface.
func (sess *serverSession) AppendLimit() uint32 {
	if sess.UserSession != nil {
		return sess.UserSession.AppendLimit()
	}
	// Default value for unauthenticated sessions
	return 104857600 // 100 MiB
}

// DiscloseLimit implements the SessionAppendLimit interface.
func (sess *serverSession) DiscloseLimit() bool {
	if sess.UserSession != nil {
		return sess.UserSession.DiscloseLimit()
	}
	// Default for unauthenticated sessions - true means we show the limit
	return true
}

func (sess *serverSession) Login(username, password string) error {
	u := sess.server.user(username)
	if u == nil {
		return imapserver.ErrAuthFailed
	}
	if err := u.Login(username, password); err != nil {
		return err
	}
	sess.UserSession = NewUserSession(u)
	return nil
}
