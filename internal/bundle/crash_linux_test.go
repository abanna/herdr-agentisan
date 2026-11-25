//go:build linux

package bundle_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
)

func TestCallerSIGKILLStopsBundleGroup(t *testing.T) {
	if dir := os.Getenv("BUNDLE_CRASH_DIR"); dir != "" {
		runCrashCaller(t, dir, os.Getenv("BUNDLE_CRASH_PHASE"))
		return
	}
	t.Parallel()
	for _, phase := range []string{"setup", "held_stdout"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			caller := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCallerSIGKILLStopsBundleGroup$")
			caller.Env = append(os.Environ(), "BUNDLE_CRASH_DIR="+dir, "BUNDLE_CRASH_PHASE="+phase)
			require.NoError(t, caller.Start())
			t.Cleanup(func() { _ = caller.Process.Kill(); _ = caller.Wait() })

			var launcher, child int
			require.Eventually(t, func() bool {
				raw, _ := os.ReadFile(filepath.Join(dir, "pids"))
				_, err := fmt.Sscan(string(raw), &launcher, &child)
				return err == nil && launcher > 1 && child > 1
			}, 4*time.Second, 20*time.Millisecond)
			for _, pid := range []int{launcher, child} {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				state, err := processState(t.Context(), pid)
				require.NoError(t, err)
				require.Regexp(t, "^[^Z]", state)
			}
			if phase == "held_stdout" {
				require.Eventually(t, func() bool {
					_, err := os.Stat(filepath.Join(dir, "ready"))
					return err == nil
				}, 4*time.Second, 20*time.Millisecond)
			}
			group, err := syscall.Getpgid(launcher)
			require.NoError(t, err)
			require.NotEqual(t, launcher, group, "the watchdog anchors the bundle group")
			life, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(group), "fd", "0"))
			require.NoError(t, err)
			require.Contains(t, life, "pipe:[")
			for _, pid := range []int{launcher, child} {
				fds := filepath.Join("/proc", strconv.Itoa(pid), "fd")
				entries, err := os.ReadDir(fds)
				require.NoError(t, err)
				for _, entry := range entries {
					file, _ := os.Readlink(filepath.Join(fds, entry.Name()))
					require.NotEqual(t, life, file, "the child inherited the lifeline")
				}
			}
			require.NoError(t, caller.Process.Kill())
			require.ErrorContains(t, caller.Wait(), "signal: killed")
			assertGone(t, launcher)
			assertGone(t, child)
			assertGone(t, group)
		})
	}
}

func runCrashCaller(t *testing.T, dir, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	l := bundletest.Launch(t, dir, "hang_call")
	l.Argv = []string{"/bin/sh", "-c", `
sleep 15 &
printf '%d %d\n' "$$" "$!" > "$1"
if [ "$2" = setup ]; then wait; else exec "$3"; fi
`, "crash-probe", filepath.Join(dir, "pids"), phase, l.Argv[0]}
	s, err := bundle.Open(ctx, l)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600))
	_, _ = bundle.ReadState(s, dir)
}
