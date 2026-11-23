// Package routelock is the daemon inbox's route (ADR-001 A74, NERD-5533):
// a mutex that counts its acquisitions, so a test can tell one hold of it
// from two. It is a package of its own so that the daemon reaches the mutex
// and its count only through these methods: a relock that bypasses the
// count, or one that corrects it, does not compile, and a counted relock
// changes Holds, which the daemon's tests pin to one hold per decision.
package routelock

import (
	"sync"
	"sync/atomic"
)

// Mutex is a mutual-exclusion lock that counts how many times Lock took
// it. Its zero value is unlocked, with a count of zero. It must not be
// copied after first use.
type Mutex struct {
	mu sync.Mutex
	n  atomic.Uint64
}

// Lock takes m, waiting until it is free, and counts the acquisition.
func (m *Mutex) Lock() {
	m.mu.Lock()
	m.n.Add(1)
}

// Unlock releases m. It is a run-time error if m is not locked.
func (m *Mutex) Unlock() { m.mu.Unlock() }

// Holds is how many times Lock has taken m. Any goroutine may read it.
func (m *Mutex) Holds() uint64 { return m.n.Load() }

// Held reports whether someone holds m now, for tests. It never waits:
// when m is free it takes and releases it at once, uncounted, so it can
// neither release a hold nor keep one.
func (m *Mutex) Held() bool {
	if m.mu.TryLock() {
		m.mu.Unlock()
		return false
	}
	return true
}
