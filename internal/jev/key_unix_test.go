//go:build unix

package jev_test

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/jev"
)

// TestKeysOpenALockedFileOnce: an env file that does not answer, as one
// 1Password mounts while it is locked (a FIFO with no writer), is opened
// once however many ask: each ask goes without a key within Wait, and the
// one open, left to finish, gives the key to the first ask after the
// file answers.
func TestKeysOpenALockedFileOnce(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "typesafe.env")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	var reads atomic.Int32
	const wait = 50 * time.Millisecond
	keys := &jev.Keys{Wait: wait, Read: counted(&reads)}

	for range 5 {
		start := time.Now()
		_, err := keys.Key(t.Context(), fifo)
		require.ErrorIs(t, err, jev.ErrNoKey)
		assert.Contains(t, err.Error(), "is 1Password unlocked?")
		assert.Less(t, time.Since(start), wait+2*time.Second, "an ask waits about Wait")
	}
	assert.Equal(t, int32(1), reads.Load(), "one open for every ask")

	// 1Password unlocks: the file answers the open still waiting.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = w.WriteString(keyEq + testKey + "\n")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	var key string
	require.Eventually(t, func() bool {
		key, err = keys.Key(t.Context(), fifo)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, testKey, key)
	assert.Equal(t, int32(1), reads.Load(), "the waiting open gave the key")
}
