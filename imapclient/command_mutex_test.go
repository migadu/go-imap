package imapclient

import (
	"testing"
	"time"
)

func TestCommandMutexCanceledAcquisition(t *testing.T) {
	var mutex commandMutex
	mutex.Lock()
	cancel := make(chan struct{})
	result := make(chan bool, 1)
	go func() { result <- mutex.LockUntil(cancel) }()
	close(cancel)
	select {
	case acquired := <-result:
		if acquired {
			t.Fatal("canceled waiter acquired a held mutex")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release the waiter")
	}
	if mutex.TryLock() {
		t.Fatal("cancellation released the owner's mutex")
	}
	mutex.Unlock()
	if !mutex.TryLock() {
		t.Fatal("mutex is unavailable after its owner unlocked")
	}
	mutex.Unlock()
	if mutex.LockUntil(cancel) {
		t.Fatal("already canceled acquisition succeeded")
	}
	if !mutex.TryLock() {
		t.Fatal("already canceled acquisition consumed the mutex")
	}
	mutex.Unlock()
}
