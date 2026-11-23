package routelock_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon/routelock"
)

// TestLockCountsEachAcquisition: Lock counts, and Unlock, Held and Holds
// do not.
func TestLockCountsEachAcquisition(t *testing.T) {
	t.Parallel()
	var m routelock.Mutex
	assert.Zero(t, m.Holds(), "a zero Mutex has no holds")
	assert.False(t, m.Held(), "a zero Mutex is free")
	for i := uint64(1); i <= 3; i++ {
		m.Lock()
		assert.True(t, m.Held(), "held after Lock")
		assert.Equal(t, i, m.Holds(), "one hold per Lock")
		m.Unlock()
		assert.False(t, m.Held(), "free after Unlock")
		assert.Equal(t, i, m.Holds(), "Unlock and Held count nothing")
	}
}

// TestLockExcludes: a second Lock waits for the first hold's Unlock, and
// Held, called meanwhile, neither waits nor frees the holder's lock.
func TestLockExcludes(t *testing.T) {
	t.Parallel()
	var m routelock.Mutex
	m.Lock()
	took := make(chan struct{})
	go func() {
		defer close(took)
		m.Lock()
		m.Unlock()
	}()
	select {
	case <-took:
		t.Fatal("a second Lock took a held Mutex")
	case <-time.After(100 * time.Millisecond):
	}
	assert.True(t, m.Held(), "still held while the second Lock waits")
	m.Unlock()
	select {
	case <-took:
	case <-time.After(5 * time.Second):
		t.Fatal("the second Lock never took the Mutex once it was free")
	}
	require.False(t, m.Held())
	assert.Equal(t, uint64(2), m.Holds(), "both acquisitions are counted")
}
