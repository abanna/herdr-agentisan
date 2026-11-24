package bundle_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
)

// workDir is a worktree inside a fresh repo, and the repo.
func workDir(t *testing.T) (repo, dir string) {
	t.Helper()
	repo = t.TempDir()
	dir = filepath.Join(repo, "generations", "NERD-5790", "g001")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return repo, dir
}

// open starts a stand-in in mode under a 20 s context, and closes it.
func open(t *testing.T, repo, mode string, extra ...string) (*bundle.Session, error) {
	t.Helper()
	return openIn(t, 20*time.Second, repo, mode, extra...)
}

func openIn(t *testing.T, limit time.Duration, repo, mode string, extra ...string) (*bundle.Session, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	t.Cleanup(cancel)
	s, err := bundle.Open(ctx, bundletest.Launch(t, repo, mode, extra...))
	if s != nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

func read(t *testing.T, mode string, extra ...string) (bundle.State, error) {
	t.Helper()
	repo, dir := workDir(t)
	s, err := open(t, repo, mode, extra...)
	require.NoError(t, err)
	return bundle.ReadState(s, dir)
}

// TestReadStateDecodesTheRealReply: the stand-in's reply is the real
// 0.19.0 server's capture (outer dry_run false, nested result.state).
func TestReadStateDecodesTheRealReply(t *testing.T) {
	t.Parallel()
	st, err := read(t, "ok", bundletest.StageEnv+"=verify")
	require.NoError(t, err)
	assert.Equal(t, "verify", st.Stage)
	assert.Equal(t, "NERD-5790", st.Ticket)
	assert.Equal(t, time.Date(2026, 11, 24, 14, 0, 5, 762327000, time.UTC), st.UpdatedAt.UTC())
	assert.True(t, strings.HasSuffix(st.StateFile, "/.agentisan/state.json"), st.StateFile)

	st, err = read(t, "ok") // no step logged yet: no stage
	require.NoError(t, err)
	assert.Empty(t, st.Stage)

	_, err = read(t, "text_only") // no structuredContent: the one JSON text
	require.NoError(t, err)
}

// TestTheHandshakeIsPinned: every way the server is not the pinned one is
// ErrVersion, which is also ErrUnavailable.
func TestTheHandshakeIsPinned(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"old_version", "bad_version", "wrong_protocol", "wrong_name", "no_tools"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			_, err := open(t, t.TempDir(), mode)
			require.ErrorIs(t, err, bundle.ErrVersion)
			assert.ErrorIs(t, err, bundle.ErrUnavailable)
		})
	}
}

// TestAServerThatNeverStartsIsUnavailable: it died before answering the
// handshake, or never started.
func TestAServerThatNeverStartsIsUnavailable(t *testing.T) {
	t.Parallel()
	_, err := open(t, t.TempDir(), "early_exit")
	require.ErrorIs(t, err, bundle.ErrUnavailable)
	require.ErrorIs(t, err, bundle.ErrServerDied)

	_, err = bundle.Open(t.Context(), bundle.Launch{Argv: []string{"/nonexistent/uv"}, Dir: t.TempDir()})
	assert.ErrorIs(t, err, bundle.ErrUnavailable)
	_, err = bundle.Open(t.Context(), bundle.Launch{})
	assert.ErrorIs(t, err, bundle.ErrUnavailable)
}

// TestTheContextBoundsEveryPhase: a silent start-up and a hung call both
// end at the deadline with ErrTimeout and the child is reaped or dead.
func TestTheContextBoundsEveryPhase(t *testing.T) {
	t.Parallel()
	start := time.Now()
	_, err := openIn(t, 400*time.Millisecond, t.TempDir(), "silent_start")
	require.ErrorIs(t, err, bundle.ErrTimeout)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 10*time.Second)

	repo, dir := workDir(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	s, err := openIn(t, 2*time.Second, repo, "hang_call", bundletest.PidFileEnv+"="+pidFile)
	require.NoError(t, err)
	pid := liveChild(t, pidFile)
	_, err = bundle.ReadState(s, dir)
	require.ErrorIs(t, err, bundle.ErrTimeout)
	assertGone(t, pid)

	_, err = bundle.ReadState(s, dir) // a failed session stays failed
	assert.ErrorIs(t, err, bundle.ErrServerDied)
}

// liveChild proves the stand-in has a live child before the test signals it.
func liveChild(t *testing.T, pidFile string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(raw))
	require.NoError(t, err)
	require.Greater(t, pid, 1)
	t.Cleanup(func() {
		if state, err := processState(context.Background(), pid); err == nil && state != "" && !strings.HasPrefix(state, "Z") {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	state, err := processState(t.Context(), pid)
	require.NoError(t, err)
	require.NotEmpty(t, state)
	require.False(t, strings.HasPrefix(state, "Z"), "child is already a zombie")
	return pid
}

// processState also works on macOS. ps returns exit 1 with no output when
// a PID is gone. Other failures must not count as a dead process.
func processState(ctx context.Context, pid int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 && len(exit.Stderr) == 0 {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		state, err := processState(t.Context(), pid)
		return err == nil && (state == "" || strings.HasPrefix(state, "Z"))
	}, 2*time.Second, 20*time.Millisecond)
}

func TestACancelledContextEndsACall(t *testing.T) {
	t.Parallel()
	repo, dir := workDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := bundle.Open(ctx, bundletest.Launch(t, repo, "hang_call"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err = bundle.ReadState(s, dir)
	require.ErrorIs(t, err, bundle.ErrTimeout)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestBundleErrorsMapByCode: each stable code of an envelope, structured or
// in the JSON text, is its own sentinel.
func TestBundleErrorsMapByCode(t *testing.T) {
	t.Parallel()
	tests := map[string][]error{
		"unknown_operation": {bundle.ErrUnknownOperation, bundle.ErrUnavailable},
		"validation_failed": {bundle.ErrRefused},
		"policy_denied":     {bundle.ErrRefused},
		"operation_failed":  {bundle.ErrOperation},
		"handler_error":     {bundle.ErrOperation},
		"a_new_code":        {bundle.ErrContract},
	}
	for code, want := range tests {
		for _, mode := range []string{"envelope", "envelope_text"} {
			t.Run(code+"/"+mode, func(t *testing.T) {
				t.Parallel()
				repo, dir := workDir(t)
				s, err := open(t, repo, mode, bundletest.CodeEnv+"="+code)
				require.NoError(t, err)
				_, err = bundle.ReadState(s, dir)
				for _, w := range want {
					require.ErrorIs(t, err, w)
				}
				_, err = bundle.ReadState(s, dir) // an operation's failure leaves the session usable
				assert.NotErrorIs(t, err, bundle.ErrServerDied)
			})
		}
	}
}

func TestAServerThatDiesMidCall(t *testing.T) {
	t.Parallel()
	repo, dir := workDir(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	s, err := openIn(t, 3*time.Second, repo, "die_mid_call", bundletest.PidFileEnv+"="+pidFile)
	require.NoError(t, err)
	pid := liveChild(t, pidFile)
	_, err = bundle.ReadState(s, dir)
	require.ErrorIs(t, err, bundle.ErrServerDied)
	assert.Contains(t, err.Error(), "exit status 7")
	assertGone(t, pid)
}

func TestReplyLineLimit(t *testing.T) {
	t.Parallel()
	_, err := read(t, "at_line_limit")
	require.NoError(t, err)

	repo, dir := workDir(t)
	s, err := openIn(t, 3*time.Second, repo, "oversize_line")
	require.NoError(t, err)
	_, err = bundle.ReadState(s, dir)
	require.ErrorIs(t, err, bundle.ErrProtocol)
	require.ErrorContains(t, err, "4 MiB")
	_, err = bundle.ReadState(s, dir)
	require.ErrorIs(t, err, bundle.ErrServerDied)
}

// TestMalformedRepliesFailTheSession: not JSON, a reply to another
// request, a request of the server's own, no payload: ErrProtocol. A reply of the wrong shape: ErrContract.
func TestMalformedRepliesFailTheSession(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"garbage", "wrong_id", "server_request", "no_payload"} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			_, err := read(t, "bad_reply", bundletest.BadEnv+"="+bad)
			require.ErrorIs(t, err, bundle.ErrProtocol)
		})
	}
	for _, mode := range []string{"rpc_error", "tool_text"} { // told apart in a later PR
		_, err := read(t, mode)
		require.ErrorIs(t, err, bundle.ErrProtocol, mode)
	}
	for _, bad := range []string{"no_updated_at", "bad_stage", "wrong_op"} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			_, err := read(t, "ok", bundletest.BadEnv+"="+bad)
			require.ErrorIs(t, err, bundle.ErrContract)
		})
	}
}

// TestCloseKillsAServerThatIgnoresEOF: one that ignores EOF is killed after
// the grace, and Close twice is fine.
func TestCloseKillsAServerThatIgnoresEOF(t *testing.T) {
	t.Parallel()
	repo, _ := workDir(t)
	s, err := open(t, repo, "stubborn")
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, s.Close())
	assert.Less(t, time.Since(start), 10*time.Second)
	require.NoError(t, s.Close(), "Close twice is fine")
}

// TestAnExecuteWithNoSessionLeftFails: Execute after Close.
func TestAnExecuteAfterCloseFails(t *testing.T) {
	t.Parallel()
	repo, _ := workDir(t)
	s, err := open(t, repo, "ok")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = s.Execute("convergence.read_state", map[string]any{})
	assert.True(t, errors.Is(err, bundle.ErrServerDied))
}
