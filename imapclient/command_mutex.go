package imapclient

import "sync"

// commandMutex is a mutex whose acquisition can be interrupted when the client
// shuts down. Its zero value is ready to use. Like sync.Mutex, it must not be
// copied after first use.
type commandMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *commandMutex) init() {
	m.once.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
}

func (m *commandMutex) Lock() {
	m.LockUntil(nil)
}

func (m *commandMutex) TryLock() bool {
	m.init()
	select {
	case <-m.token:
		return true
	default:
		return false
	}
}

// LockUntil acquires the mutex, or returns false when cancel is closed. A nil
// channel waits without cancellation. The caller only unlocks a successful
// acquisition.
func (m *commandMutex) LockUntil(cancel <-chan struct{}) bool {
	m.init()
	select {
	case <-cancel:
		return false
	default:
	}
	select {
	case <-cancel:
		return false
	case <-m.token:
		// Cancellation and acquisition can become ready together. Prefer
		// cancellation, returning ownership so another waiter can proceed.
		select {
		case <-cancel:
			m.Unlock()
			return false
		default:
			return true
		}
	}
}

func (m *commandMutex) Unlock() {
	m.init()
	select {
	case m.token <- struct{}{}:
	default:
		panic("imapclient: unlock of unlocked command mutex")
	}
}
