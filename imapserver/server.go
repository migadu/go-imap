// Package imapserver implements an IMAP server.
package imapserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
)

var errClosed = errors.New("imapserver: server closed")

// Logger is a facility to log error messages.
type Logger interface {
	Printf(format string, args ...interface{})
}

// Options contains server options.
//
// The only required field is NewSession.
type Options struct {
	// NewSession is called when a client connects.
	NewSession func(*Conn) (Session, *GreetingData, error)
	// Supported capabilities. If nil, only IMAP4rev1 is advertised. This set
	// must contain at least IMAP4rev1 or IMAP4rev2.
	//
	// The following capabilities are part of IMAP4rev2 and need to be
	// explicitly enabled by IMAP4rev1-only servers:
	//
	//   - NAMESPACE
	//   - UIDPLUS
	//   - ESEARCH
	//   - LIST-EXTENDED
	//   - LIST-STATUS
	//   - MOVE
	//   - STATUS=SIZE
	Caps imap.CapSet
	// Logger is a logger to print error messages. If nil, log.Default is used.
	Logger Logger
	// TLSConfig is a TLS configuration for STARTTLS. If nil, STARTTLS is
	// disabled.
	TLSConfig *tls.Config
	// InsecureAuth allows clients to authenticate without TLS. In this mode,
	// the server is susceptible to man-in-the-middle attacks.
	InsecureAuth bool
	// IsTLS is called to determine if a connection should be considered
	// TLS-secured for authentication purposes. This is useful when TLS is
	// terminated by a reverse proxy and the server receives a plain TCP
	// connection. If nil, the default check using type assertion is used.
	// Note: This does NOT affect STARTTLS behavior.
	IsTLS func(net.Conn) bool
	// Raw ingress and egress data will be written to this writer, if any.
	// Note, this may include sensitive information such as credentials used
	// during authentication.
	DebugWriter io.Writer
}

func (options *Options) wrapReadWriter(rw io.ReadWriter) io.ReadWriter {
	if options.DebugWriter == nil {
		return rw
	}
	return struct {
		io.Reader
		io.Writer
	}{
		Reader: io.TeeReader(rw, options.DebugWriter),
		Writer: io.MultiWriter(rw, options.DebugWriter),
	}
}

func (options *Options) caps() imap.CapSet {
	if options.Caps != nil {
		return options.Caps
	}
	return imap.CapSet{imap.CapIMAP4rev1: {}}
}

// Server is an IMAP server.
type Server struct {
	options Options

	listenerWaitGroup sync.WaitGroup
	connsWaitGroup    sync.WaitGroup

	mutex     sync.Mutex
	listeners map[net.Listener]struct{}
	conns     map[*Conn]struct{}
	closed    bool
}

// New creates a new server.
func New(options *Options) *Server {
	if caps := options.caps(); !caps.Has(imap.CapIMAP4rev2) && !caps.Has(imap.CapIMAP4rev1) {
		panic("imapserver: at least IMAP4rev1 must be supported")
	}
	return &Server{
		options:   *options,
		listeners: make(map[net.Listener]struct{}),
		conns:     make(map[*Conn]struct{}),
	}
}

func (s *Server) logger() Logger {
	if s.options.Logger == nil {
		return log.Default()
	}
	return s.options.Logger
}

// Serve accepts incoming connections on the listener ln.
func (s *Server) Serve(ln net.Listener) error {
	s.mutex.Lock()
	ok := !s.closed
	if ok {
		s.listeners[ln] = struct{}{}
	}
	s.mutex.Unlock()
	if !ok {
		return errClosed
	}

	defer func() {
		s.mutex.Lock()
		delete(s.listeners, ln)
		s.mutex.Unlock()
	}()

	s.listenerWaitGroup.Add(1)
	defer s.listenerWaitGroup.Done()

	var delay time.Duration
	for {
		conn, err := ln.Accept()
		var temporary interface {
			Temporary() bool
		}
		if errors.As(err, &temporary) && temporary.Temporary() {
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else {
				delay *= 2
			}
			if max := 1 * time.Second; delay > max {
				delay = max
			}
			s.logger().Printf("accept error (retrying in %v): %v", delay, err)
			time.Sleep(delay)
			continue
		} else if errors.Is(err, net.ErrClosed) {
			return nil
		} else if err != nil {
			return fmt.Errorf("accept error: %w", err)
		}

		delay = 0
		s.connsWaitGroup.Add(1)
		go func() {
			defer s.connsWaitGroup.Done()
			newConn(conn, s).serve()
		}()
	}
}

// ListenAndServe listens on the TCP network address addr and then calls Serve.
//
// If addr is empty, ":143" is used.
func (s *Server) ListenAndServe(addr string) error {
	if addr == "" {
		addr = ":143"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// ListenAndServeTLS listens on the TCP network address addr and then calls
// Serve to handle incoming TLS connections.
//
// The TLS configuration set in Options.TLSConfig is used. If addr is empty,
// ":993" is used.
func (s *Server) ListenAndServeTLS(addr string) error {
	if addr == "" {
		addr = ":993"
	}
	ln, err := tls.Listen("tcp", addr, s.options.TLSConfig)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Close immediately closes all active listeners and connections.
//
// Close returns any error returned from closing the server's underlying
// listeners.
//
// Once Close has been called on a server, it may not be reused; future calls
// to methods such as Serve will return an error.
func (s *Server) Close() error {
	var err error

	s.mutex.Lock()
	ok := !s.closed
	if ok {
		s.closed = true
		for l := range s.listeners {
			if closeErr := l.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
	}
	s.mutex.Unlock()
	if !ok {
		return errClosed
	}

	s.listenerWaitGroup.Wait()

	s.forceCloseConns()

	return err
}

// Shutdown gracefully shuts down the server without interrupting any
// active connections. Shutdown works by first closing all open listeners,
// then waiting for all connections to close or the context to expire,
// whichever comes first.
//
// When Shutdown is called, Serve immediately returns. Make sure the
// program doesn't exit and waits instead for Shutdown to return.
//
// If the provided context expires before the shutdown completes, Shutdown
// force-closes any remaining connections and returns the context's error.
// Otherwise it returns nil.
//
// Once Shutdown has been called on a server, it may not be reused.
func (s *Server) Shutdown(ctx context.Context) error {
	// 1. Stop accepting new connections
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return errClosed
	}
	s.closed = true
	var listenerErr error
	for l := range s.listeners {
		if err := l.Close(); err != nil && listenerErr == nil {
			listenerErr = err
		}
	}
	s.mutex.Unlock()

	// 2. Wait for all listeners and connections to finish processing
	done := make(chan struct{})
	go func() {
		s.listenerWaitGroup.Wait()
		s.connsWaitGroup.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		// Context expired - force-close all remaining connections
		s.forceCloseConns()
		return ctx.Err()
	case <-done:
		// All connections closed gracefully
		return listenerErr
	}
}

func (s *Server) forceCloseConns() {
	s.mutex.Lock()
	for c := range s.conns {
		c.mutex.Lock()
		c.conn.Close()
		c.mutex.Unlock()
	}
	s.mutex.Unlock()
}
