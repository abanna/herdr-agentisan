//go:build unix

package settings_test

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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// fakeOpScript stands in for the 1Password CLI. It checks it was called as
// `op read --no-newline <ref>` and answers by reference. It runs with a PATH
// holding nothing but itself, so it calls other programs by absolute path.
const fakeOpScript = `#!/bin/sh
if [ "$#" -ne 3 ] || [ "$1" != read ] || [ "$2" != --no-newline ]; then
	echo "unexpected arguments: $*" >&2
	exit 2
fi
case "$3" in
op://Vault/Item/ok) printf '%s' fake-op-value ;;
op://Vault/Item/empty) ;;
op://Vault/Item/env) printf '%s' "$FAKE_OP_VALUE" ;;
"op://Coffre/Café/mot") printf '%s' accent-ok ;;
"op://Vault/Item with space/ok") printf '%s' space-ok ;;
op://Vault/Item/framed) printf 'line1\r\nline2\n' ;;
op://Vault/Item/stdio)
	if [ -t 0 ] || [ -t 1 ] || [ -t 2 ]; then echo "a terminal" >&2; exit 4; fi
	if read -r line; then echo "stdin held: $line" >&2; exit 3; fi
	printf '%s' no-terminal ;;
op://Vault/Item/loud)
	i=0
	while [ "$i" -lt 2048 ]; do
		printf '%063d\n' 0
		printf '%063d\n' 0 >&2
		i=$((i + 1))
	done ;;
op://Vault/Item/stderr) printf '%s\n' "$FAKE_OP_STDERR" >&2; exit 1 ;;
op://Vault/Item/hang) exec /bin/sleep 30 ;;
op://Vault/Item/spawn) /bin/sleep 30 & echo "$!" > "$FAKE_OP_PIDFILE"; wait ;;
*) echo "[ERROR] could not read secret: item not found" >&2; exit 1 ;;
esac
`

// installFakeOp writes the fake op into a new directory and returns it.
func installFakeOp(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	// #nosec G306 -- the fake op must be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "op"), []byte(fakeOpScript), 0o700))
	return bin
}

// gone reports whether pid has exited: no such process, or a zombie only
// its parent has yet to reap.
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	_, after, ok := strings.Cut(string(stat), ") ")
	return ok && strings.HasPrefix(after, "Z")
}

// TestOpRead runs OpRead against a fake op on a PATH holding nothing else,
// so the real 1Password CLI is never reachable. It sets PATH, so it cannot
// run in parallel.
func TestOpRead(t *testing.T) {
	assert.Equal(t, 10*time.Second, settings.OpTimeout)
	t.Setenv("PATH", installFakeOp(t))

	tests := map[string]struct {
		ref   string
		env   map[string]string
		ctx   func(t *testing.T) context.Context
		want  string
		check func(t *testing.T, got string, err error, elapsed time.Duration)
	}{
		"reads the value":                {ref: "op://Vault/Item/ok", want: "fake-op-value"},
		"an empty value is no error":     {ref: "op://Vault/Item/empty", want: ""},
		"the value is returned verbatim": {ref: "op://Vault/Item/framed", want: "line1\r\nline2\n"},
		"the child inherits the environment": {
			ref: "op://Vault/Item/env", env: map[string]string{"FAKE_OP_VALUE": "from-the-environment"}, want: "from-the-environment",
		},
		"op sees no terminal and a closed stdin":             {ref: "op://Vault/Item/stdio", want: "no-terminal"},
		"a non-ASCII reference reaches op whole":             {ref: "op://Coffre/Café/mot", want: "accent-ok"},
		"a reference with spaces reaches op as one argument": {ref: "op://Vault/Item with space/ok", want: "space-ok"},
		"a reference past the argv limit fails to start": {
			ref: "op://Vault/Item/" + strings.Repeat("x", 2<<20),
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.ErrorIs(t, err, syscall.E2BIG)
			},
		},
		"invalid UTF-8 in stderr is dropped": {
			ref: "op://Vault/Item/stderr", env: map[string]string{"FAKE_OP_STDERR": "bad \xff byte"},
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.Error(t, err)
				assert.True(t, utf8.ValidString(err.Error()), "error is valid UTF-8: %q", err.Error())
				assert.Contains(t, err.Error(), "bad  byte")
			},
		},
		"a CRLF stderr line loses its CR": {
			ref: "op://Vault/Item/stderr", env: map[string]string{"FAKE_OP_STDERR": "first line\r"},
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.Error(t, err)
				assert.True(t, strings.HasSuffix(err.Error(), "first line"), "error: %q", err.Error())
			},
		},
		"output past the pipe buffer on both streams": {
			ref: "op://Vault/Item/loud",
			check: func(t *testing.T, got string, err error, _ time.Duration) {
				t.Helper()
				require.NoError(t, err)
				assert.Len(t, got, 2048*64)
			},
		},
		"a non-zero exit carries op's stderr": {
			ref: "op://Vault/Item/gone",
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "item not found")
			},
		},
		"a stderr line of exactly 200 bytes is kept whole": {
			ref: "op://Vault/Item/stderr", env: map[string]string{"FAKE_OP_STDERR": strings.Repeat("x", 199) + "y"},
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.Error(t, err)
				assert.Contains(t, err.Error(), strings.Repeat("x", 199)+"y")
			},
		},
		"a stderr line past 200 bytes is cut": {
			ref: "op://Vault/Item/stderr", env: map[string]string{"FAKE_OP_STDERR": strings.Repeat("x", 200) + "z"},
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.Error(t, err)
				assert.Contains(t, err.Error(), strings.Repeat("x", 200))
				assert.NotContains(t, err.Error(), "xz")
			},
		},
		"the cut never splits a character": {
			ref: "op://Vault/Item/stderr", env: map[string]string{"FAKE_OP_STDERR": strings.Repeat("x", 199) + "é"},
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.Error(t, err)
				assert.True(t, utf8.ValidString(err.Error()), "error is valid UTF-8: %q", err.Error())
				assert.True(t, strings.HasSuffix(err.Error(), strings.Repeat("x", 199)), "error: %q", err.Error())
			},
		},
		"a canceled context never starts op": {
			ref: "op://Vault/Item/ok",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			check: func(t *testing.T, _ string, err error, _ time.Duration) {
				t.Helper()
				require.ErrorIs(t, err, context.Canceled)
			},
		},
		"a cancel while op runs stops it": {
			ref: "op://Vault/Item/hang",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				time.AfterFunc(200*time.Millisecond, cancel)
				return ctx
			},
			check: func(t *testing.T, _ string, err error, elapsed time.Duration) {
				t.Helper()
				require.ErrorIs(t, err, context.Canceled)
				assert.Less(t, elapsed, 5*time.Second)
			},
		},
		"a deadline stops a hung op and reports a timeout": {
			ref: "op://Vault/Item/hang",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			check: func(t *testing.T, _ string, err error, elapsed time.Duration) {
				t.Helper()
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Less(t, elapsed, 5*time.Second)
			},
		},
		"a timed-out op leaves no descendant running": {
			ref: "op://Vault/Item/spawn",
			ctx: func(t *testing.T) context.Context {
				t.Setenv("FAKE_OP_PIDFILE", filepath.Join(t.TempDir(), "pid"))
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			check: func(t *testing.T, _ string, err error, elapsed time.Duration) {
				t.Helper()
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Less(t, elapsed, 5*time.Second)
				raw, readErr := os.ReadFile(os.Getenv("FAKE_OP_PIDFILE"))
				require.NoError(t, readErr)
				pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
				require.NoError(t, convErr)
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				assert.Eventually(t, func() bool { return gone(pid) }, 3*time.Second, 20*time.Millisecond,
					"op's child %d outlived it", pid)
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}
			start := time.Now()
			got, err := settings.OpRead(ctx, tc.ref)
			elapsed := time.Since(start)
			if tc.check != nil {
				tc.check(t, got, err, elapsed)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestOpReadRejectsANonReference: a value that is not an op:// reference is
// never handed to op, where it could be read as a flag.
func TestOpReadRejectsANonReference(t *testing.T) {
	t.Setenv("PATH", installFakeOp(t))

	for name, ref := range map[string]string{
		"an option":    "--help",
		"a bare word":  "secret",
		"a short path": "op://Vault/Item",
		"a NUL":        "op://Vault/Item/ok\x00",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := settings.OpRead(t.Context(), ref)
			require.ErrorIs(t, err, settings.ErrSecretValue)
		})
	}
}

// TestOpReadStartClasses covers how op is found: through PATH only, following
// a symlink, and failing before any child exists when it cannot start.
func TestOpReadStartClasses(t *testing.T) {
	tests := map[string]struct {
		setup   func(t *testing.T) // sets PATH
		nonRoot bool
		ok      bool
		wantErr error
	}{
		"op on PATH is a symlink": {
			setup: func(t *testing.T) {
				link := t.TempDir()
				require.NoError(t, os.Symlink(filepath.Join(installFakeOp(t), "op"), filepath.Join(link, "op")))
				t.Setenv("PATH", link)
			},
			ok: true,
		},
		"op is not on PATH": {setup: func(t *testing.T) { t.Setenv("PATH", t.TempDir()) }, wantErr: exec.ErrNotFound},
		"PATH is empty":     {setup: func(t *testing.T) { t.Setenv("PATH", "") }, wantErr: exec.ErrNotFound},
		"PATH is unset": {
			setup: func(t *testing.T) {
				t.Setenv("PATH", "")
				require.NoError(t, os.Unsetenv("PATH"))
			},
			wantErr: exec.ErrNotFound,
		},
		"op is not executable": {
			setup: func(t *testing.T) {
				bin := installFakeOp(t)
				require.NoError(t, os.Chmod(filepath.Join(bin, "op"), 0o600))
				t.Setenv("PATH", bin)
			},
			nonRoot: true, wantErr: exec.ErrNotFound,
		},
		"op is a directory": {
			setup: func(t *testing.T) {
				bin := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(bin, "op"), 0o700))
				t.Setenv("PATH", bin)
			},
			wantErr: exec.ErrNotFound,
		},
		"op is a dangling symlink": {
			setup: func(t *testing.T) {
				bin := t.TempDir()
				require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(bin, "op")))
				t.Setenv("PATH", bin)
			},
			wantErr: exec.ErrNotFound,
		},
		"op is a FIFO with execute bits": {
			setup: func(t *testing.T) {
				bin := t.TempDir()
				require.NoError(t, syscall.Mkfifo(filepath.Join(bin, "op"), 0o700))
				t.Setenv("PATH", bin)
			},
			wantErr: syscall.EACCES,
		},
		"op only in a relative PATH entry": {
			setup: func(t *testing.T) {
				t.Chdir(installFakeOp(t))
				t.Setenv("PATH", ".")
			},
			wantErr: exec.ErrDot,
		},
		"the PATH entry cannot be searched": {
			setup: func(t *testing.T) {
				bin := installFakeOp(t)
				require.NoError(t, os.Chmod(bin, 0o000))
				t.Cleanup(func() { _ = os.Chmod(bin, 0o700) })
				t.Setenv("PATH", bin)
			},
			nonRoot: true, wantErr: exec.ErrNotFound,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.nonRoot && os.Geteuid() == 0 {
				t.Skip("root ignores permission bits")
			}
			tc.setup(t)
			got, err := settings.OpRead(t.Context(), "op://Vault/Item/ok")
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, "fake-op-value", got)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.Contains(t, err.Error(), "op read")
		})
	}
}
