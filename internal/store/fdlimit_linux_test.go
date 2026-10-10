package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// fdHelperEnv switches the test binary into the descriptor-limit helper. The
// limit is process-wide, which is why it runs in a process of its own.
const fdHelperEnv = "STORE_TEST_FD_HELPER"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fdHelperEnv); dir != "" {
		helperFDLimit(dir)
		return
	}
	os.Exit(m.Run())
}

// fdResult is what Open did with room for Extra more descriptors.
type fdResult struct {
	Extra  int    `json:"extra"`
	Err    string `json:"err"` // the sentinel's text, "" when Open succeeded
	Leaked int    `json:"leaked"`
}

// openFDs lists the descriptors open now. The one ReadDir reads the
// listing through is closed again when it returns, so each number is
// re-checked with fstat.
func openFDs() []int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	var fds []int
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) == nil {
			fds = append(fds, fd)
		}
	}
	return fds
}

// limitFor is the RLIMIT_NOFILE value that leaves exactly room descriptor
// numbers free. The limit caps a new descriptor's number, not how many are
// open: a process that inherited a high descriptor (a CI runner passes
// several) has free numbers below it, which the open count plus room would
// let Open use.
func limitFor(open []int, room int) uint64 {
	used := make(map[int]bool, len(open))
	for _, fd := range open {
		used[fd] = true
	}
	free := 0
	for n := 0; ; n++ {
		if used[n] {
			continue
		}
		if free == room {
			return uint64(n) //nolint:gosec // a small non-negative descriptor number
		}
		free++
	}
}

// helperFDLimit opens a fresh database with the descriptor limit set to
// leave 0, 1, 2 and so on free descriptor numbers, and reports, for each,
// how Open ended and how many descriptors it left open.
func helperFDLimit(dir string) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		os.Exit(3)
	}
	// One open first, so anything the runtime or the driver opens once per
	// process is open before counting starts.
	if s, err := store.Open(context.Background(), filepath.Join(dir, "warm.db")); err == nil {
		_ = s.Close()
	}
	var out []fdResult
	for extra := range 12 {
		before := openFDs()
		capped := lim
		capped.Cur = limitFor(before, extra)
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &capped); err != nil {
			os.Exit(4)
		}
		s, err := store.Open(context.Background(), filepath.Join(dir, fmt.Sprintf("s%d.db", extra)))
		if err == nil {
			_ = s.Close()
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			os.Exit(5)
		}
		r := fdResult{Extra: extra, Leaked: len(openFDs()) - len(before)}
		for _, sentinel := range []error{store.ErrOpen, store.ErrPragma, store.ErrMigration, store.ErrStore} {
			if err != nil && errors.Is(err, sentinel) {
				r.Err = sentinel.Error()
				break
			}
		}
		if err != nil && r.Err == "" {
			r.Err = "unexpected: " + err.Error()
		}
		out = append(out, r)
	}
	raw, _ := json.Marshal(out)
	_, _ = os.Stdout.Write(raw)
	os.Exit(0)
}

// TestOpenReleasesDescriptorsAtTheLimit: a process at its descriptor limit
// (EMFILE) fails Open with one of the store's sentinels wherever in Open the
// limit is hit, and leaves no descriptor open; with room, Open succeeds.
func TestOpenReleasesDescriptorsAtTheLimit(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^$") //nolint:gosec // re-execs this test binary as a helper
	cmd.Env = append(os.Environ(), fdHelperEnv+"="+t.TempDir())
	raw, err := cmd.Output()
	require.NoError(t, err, "helper failed")
	var results []fdResult
	require.NoError(t, json.Unmarshal(raw, &results))
	require.NotEmpty(t, results)

	failed := 0
	for _, r := range results {
		assert.Zero(t, r.Leaked, "room for %d more descriptors: %d left open", r.Extra, r.Leaked)
		if r.Err != "" {
			failed++
			assert.Contains(t, []string{store.ErrOpen.Error(), store.ErrPragma.Error(), store.ErrMigration.Error()}, r.Err,
				"room for %d more descriptors", r.Extra)
		}
	}
	assert.Equal(t, store.ErrOpen.Error(), results[0].Err, "with no room, the first open fails")
	assert.Empty(t, results[len(results)-1].Err, "with room, Open succeeds")
	assert.Positive(t, failed)
	t.Logf("results: %+v", results)
}
